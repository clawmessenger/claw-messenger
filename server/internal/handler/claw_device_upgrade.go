package handler

// 设备端（supervisor）升级通道 —— 与启停共用同一条轮询，不新增 IM 通道。
//
// 为什么复用 /api/claw/device/control 这一次往返：supervisor 本来就每 10 秒来问
// 「我该跑哪些 agent」（main.ts 的 SUPERVISOR_CONTROL_POLL_MS），exe 版本是**机器**
// 属性（install.ts 是单机单路径），而 supervisor 是唯一的机器级进程 —— 于是
// 「上报版本」和「领取升级指令」天然可以塞进同一个请求/响应：零新增定时器、
// 零新增通道，离线设备上线后第一轮轮询就能领到指令（与启停同样的语义）。
//
// 心跳不能承担这件事：心跳只有 agent 子进程发（main.ts 的 30s heartbeat），
// 服务端 NodeService.Heartbeat 只写进程内 map 不落库，多副本还会分片 —— 拿它
// 存版本等于把「一台机器一个版本」写成一机 N 行、还会在重启后丢失。
//
// 设备侧的进度（downloading/verifying/...）走单独的 report 接口：一次下载要几十
// 秒到几分钟，阶段之间不可能等到下一个 10 秒轮询才回传，页面就会长时间停在
// 「下载中」看不出区别。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 升级状态的取值集合与迁移 601 的 CHECK 一致。
const (
	clawUpgradeStatusPending     = "pending"
	clawUpgradeStatusWaitingIdle = "waiting_idle"
	clawUpgradeStatusDownloading = "downloading"
	clawUpgradeStatusVerifying   = "verifying"
	clawUpgradeStatusRestarting  = "restarting"
	clawUpgradeStatusSucceeded   = "succeeded"
	clawUpgradeStatusFailed      = "failed"
	clawUpgradeStatusCancelled   = "cancelled"
)

// 设备可回传的状态（” 与 pending 都是服务端写入的，设备不得自己回到 pending）。
var clawUpgradeDeviceReportable = map[string]bool{
	clawUpgradeStatusWaitingIdle: true,
	clawUpgradeStatusDownloading: true,
	clawUpgradeStatusVerifying:   true,
	clawUpgradeStatusRestarting:  true,
	clawUpgradeStatusSucceeded:   true,
	clawUpgradeStatusFailed:      true,
	clawUpgradeStatusCancelled:   true,
}

// clawNodeUpgrade 是 rongcloud_node_upgrade 的一行（机器维度）。
type clawNodeUpgrade struct {
	MachineID        string
	NodeID           pgtype.UUID
	WorkspaceID      pgtype.UUID
	ReportedVersion  string
	ReportedPlatform string
	// ReportedHostname 是设备操作系统的 hostname（人可读的机器标识）。与「机器名」
	// 的区别：机器名取自机器主节点的昵称（可被人改），hostname 是系统事实。
	ReportedHostname   string
	ReportedAt         pgtype.Timestamptz
	ReportedGeneration int64
	TargetVersion      string
	DesiredGeneration  int64
	Status             string
	ErrorCode          string
	ErrorDetail        string
	Force              bool
	BatchID            string
	Wave               int
	RequestedBy        string
	RequestedAt        pgtype.Timestamptz
	UpdatedAt          time.Time
}

// clawDeviceUpgradeIntent 是下发给 supervisor 的升级指令。
//
// 只带版本号 + 校验和 + 下载地址列表，**不带**任何「服务端自己拍出来的路径」：
// mirrors 里的相对路径由设备拼上它自己的 serverUrl（设备本来就知道自己的服务端
// 地址），服务端因此不需要配置公网域名。
type clawDeviceUpgradeIntent struct {
	Generation    int64    `json:"generation"`
	TargetVersion string   `json:"targetVersion"`
	AssetName     string   `json:"assetName"`
	SHA256        string   `json:"sha256"`
	Force         bool     `json:"force"`
	Mirrors       []string `json:"mirrors"`
}

// machineIDForDevice 与 AgentNodesForDevice 的机器键推导逐字一致
// （node_device_auth.go）：machine_id 为空的老设备用融云 id 后缀兜底。两处必须
// 得出同一个键，否则「设备上报的版本」和「管理端点的那一行」会落到两行上。
func machineIDForDevice(machineNode db.RongcloudNode) string {
	if machineNode.MachineID != "" {
		return machineNode.MachineID
	}
	return strings.TrimPrefix(machineNode.RongcloudUserID, "rc_node_")
}

// clawDeviceRecordUpgradeState 把设备上报的版本/平台/主机名写进机器行，同时把
// reported_generation 单调推进（GREATEST：乱序到达的旧轮询不能把它退回去）。
// 不碰 status/target —— 那是管理端与设备进度的领域，版本上报只负责版本。
//
// hostname 传空表示「这次没带」（老版本设备），此时**保留库里已有的值**：否则
// 一台已上报过主机名的机器会被它自己的旧版本心跳擦成空。
func (h *Handler) clawDeviceRecordUpgradeState(
	ctx context.Context,
	machineNode db.RongcloudNode,
	version, platform, hostname string,
	generation int64,
) error {
	if h.DB == nil {
		return errors.New("db unavailable")
	}
	_, err := h.DB.Exec(ctx, `
		INSERT INTO rongcloud_node_upgrade
			(machine_id, node_id, workspace_id, reported_version, reported_platform,
			 reported_hostname, reported_at, reported_generation, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, now())
		ON CONFLICT (machine_id) DO UPDATE
		   SET node_id = EXCLUDED.node_id,
		       workspace_id = EXCLUDED.workspace_id,
		       reported_version = EXCLUDED.reported_version,
		       reported_platform = EXCLUDED.reported_platform,
		       reported_hostname = COALESCE(
		           NULLIF(EXCLUDED.reported_hostname, ''),
		           rongcloud_node_upgrade.reported_hostname),
		       reported_at = now(),
		       reported_generation = GREATEST(
		           rongcloud_node_upgrade.reported_generation, EXCLUDED.reported_generation),
		       updated_at = now()`,
		machineIDForDevice(machineNode), machineNode.ID, machineNode.WorkspaceID,
		version, platform, hostname, generation)
	return err
}

// clawNodeUpgradeColumns 是读升级行的唯一列清单：三处查询（单台/批量/全量）
// 必须逐字一致，否则 scanClawNodeUpgrade 会在某一处静默错位。
const clawNodeUpgradeColumns = `
	machine_id, node_id, workspace_id,
	reported_version, reported_platform, reported_hostname, reported_at, reported_generation,
	target_version, desired_generation, status, error_code, error_detail,
	force, batch_id, wave, requested_by, requested_at, updated_at`

// clawNodeUpgradeByMachine 读一台机器的升级行；没有行时 found=false。
func (h *Handler) clawNodeUpgradeByMachine(ctx context.Context, machineID string) (clawNodeUpgrade, bool, error) {
	if h.DB == nil || machineID == "" {
		return clawNodeUpgrade{}, false, nil
	}
	row := h.DB.QueryRow(ctx, `SELECT `+clawNodeUpgradeColumns+`
		  FROM rongcloud_node_upgrade WHERE machine_id = $1`, machineID)
	upgrade, err := scanClawNodeUpgrade(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return clawNodeUpgrade{}, false, nil
	}
	if err != nil {
		return clawNodeUpgrade{}, false, err
	}
	return upgrade, true, nil
}

func scanClawNodeUpgrade(row pgx.Row) (clawNodeUpgrade, error) {
	var up clawNodeUpgrade
	err := row.Scan(&up.MachineID, &up.NodeID, &up.WorkspaceID,
		&up.ReportedVersion, &up.ReportedPlatform, &up.ReportedHostname, &up.ReportedAt,
		&up.ReportedGeneration,
		&up.TargetVersion, &up.DesiredGeneration, &up.Status, &up.ErrorCode, &up.ErrorDetail,
		&up.Force, &up.BatchID, &up.Wave, &up.RequestedBy, &up.RequestedAt, &up.UpdatedAt)
	return up, err
}

// clawUpgradeTerminal 判断某个状态是否已经结束（不会再自己往前走）。
func clawUpgradeTerminal(status string) bool {
	switch status {
	case clawUpgradeStatusSucceeded, clawUpgradeStatusFailed, clawUpgradeStatusCancelled:
		return true
	default:
		return false
	}
}

// upgradeWaveOpen 是批量波次的放行判据：同批次里任何更低编号的波次还有活跃
// 机器时，本波次不下发。纯函数，便于单测（DB 那份计数在
// clawDeviceUpgradePending).
func upgradeWaveOpen(earlierActive int) bool {
	return earlierActive == 0
}

// clawUpgradeWaveLease 是「一台机器占着波次多久算失联」。
//
// 波次闸没有后台调度器（设备来问一次算一次），所以一台永远不来领的机器（离线、
// 关机、平台不支持）会把它后面的所有波次**永久**堵死 —— 20 台里 1 台不来，另外
// 19 台就永远拿不到指令，而页面上只会显示「排队中」（审查报告 S3）。超时后不再
// 计入闸门：后续波次照常放行，那一台自己上线时仍然能领到（它的波次对它自己
// 永远是开的）。
const clawUpgradeWaveLease = 10 * time.Minute

// clawUpgradeBlocksWave 判断一条升级行是否还在挡住后面的波次。纯函数，表驱动
// 单测钉住三个边界：终态不挡、租约过期不挡、其余都挡。
func clawUpgradeBlocksWave(status string, requestedAt, now time.Time) bool {
	if status == "" || clawUpgradeTerminal(status) {
		return false
	}
	if requestedAt.IsZero() {
		// 没有下发时刻（老数据）：保守地认为它还在挡，否则波次保护会静默失效。
		return true
	}
	return now.Sub(requestedAt) <= clawUpgradeWaveLease
}

// clawDeviceUpgradePending 数一数同批次更低波次还有几台真正在挡路 —— 这就是
// 「波次放行」的全部实现：没有后台调度器，设备来问一次算一次。
//
// 计数在 Go 里做而不是写进 SQL：租约判定要能被单测覆盖，而 `now()` 落在 SQL 里
// 就只能靠集成测试。
func (h *Handler) clawDeviceUpgradePending(ctx context.Context, batchID string, wave int) (int, error) {
	if h.DB == nil || batchID == "" || wave <= 1 {
		return 0, nil
	}
	rows, err := h.DB.Query(ctx, `
		SELECT status, COALESCE(requested_at, created_at)
		  FROM rongcloud_node_upgrade
		 WHERE batch_id = $1 AND wave < $2`, batchID, wave)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	now := time.Now()
	pending := 0
	for rows.Next() {
		var status string
		var requestedAt time.Time
		if err := rows.Scan(&status, &requestedAt); err != nil {
			return 0, err
		}
		if clawUpgradeBlocksWave(status, requestedAt, now) {
			pending++
		}
	}
	return pending, rows.Err()
}

// clawDeviceUpgradeFailPlatform 把一条「这台机器升不了」的行收尾。
//
// 条件更新（desired_generation 必须仍是我们读到的那一次、状态仍是待执行），所以
// 即使管理员此刻又下发了一次也不会被这条覆盖。写完之后这一行是终态，波次闸不再
// 把它算作「在挡路」，页面上也变成一条能读懂的 failed。
func (h *Handler) clawDeviceUpgradeFailPlatform(ctx context.Context, machineID string, upgrade clawNodeUpgrade) {
	if h.DB == nil {
		return
	}
	code := "platform_unsupported"
	detail := fmt.Sprintf("支持的是 %s，本机上报的是 %s", clawUpgradeSupportedPlatformsText(), upgrade.ReportedPlatform)
	if upgrade.ReportedPlatform == "" {
		code = "platform_unknown"
		detail = "设备还没有上报 exe 平台（1.3.0 之前的版本不会上报），需要先在设备上手动安装一次"
	}
	if _, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_node_upgrade
		   SET status = $3, error_code = $4, error_detail = $5, updated_at = now()
		 WHERE machine_id = $1 AND desired_generation = $2
		   AND status IN ($6, $7)`,
		machineID, upgrade.DesiredGeneration, clawUpgradeStatusFailed, code, detail,
		clawUpgradeStatusPending, clawUpgradeStatusWaitingIdle); err != nil {
		slog.Warn("claw: failed to close an unsupported-platform upgrade",
			"machine", machineID, "error", err)
	}
}

// clawDeviceUpgradeCompleteInPlace 把一条「设备已经是目标版本」的行收尾成 succeeded。
//
// 为什么服务端要自己收尾（审查报告 R5）：设备侧上报 succeeded 有两条路径 ——
//
//	(a) 新 supervisor 跑完交换后、按账本里的 generation 上报 succeeded；
//	(b) 设备拿到 intent 后发现「我早就是目标版本了」就补报一次。
//
// 而 (b) 这条路径在下发阶段就被本文件的短路分支挡住了（不重复下发 88MB），于是
// 当 (a) 因为账本写坏 / 断电 / 进程被杀而**一次都没发出去**时，服务端状态会永久
// 停在 downloading/restarting，而且不会自愈 —— 页面上就是「卡住了」。
//
// 这里能安全地自己收尾，是因为三个事实都在**同一个请求**里：target_version 来自
// 升级行、current_version 来自本次请求、当前状态也刚读出来。所以刻意**不去**读库
// 比较 reported_version —— 那个字段会被 10 秒一次的心跳（generation=0）并发写，
// 读出来比较只会打架。
//
// 条件更新（desired_generation 仍是读到的那次 + 状态仍非终态），所以管理员此刻
// 又下发了一次的话不会被这条覆盖。
func (h *Handler) clawDeviceUpgradeCompleteInPlace(ctx context.Context, machineID string, upgrade clawNodeUpgrade, currentVersion string) {
	if h.DB == nil {
		return
	}
	if _, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_node_upgrade
		   SET status = $3, error_code = '', error_detail = '', updated_at = now()
		 WHERE machine_id = $1 AND desired_generation = $2
		   AND status NOT IN ($4, $5, $6)`,
		machineID, upgrade.DesiredGeneration, clawUpgradeStatusSucceeded,
		clawUpgradeStatusSucceeded, clawUpgradeStatusFailed, clawUpgradeStatusCancelled); err != nil {
		slog.Warn("claw: failed to close an already-installed upgrade",
			"machine", machineID, "version", currentVersion, "error", err)
		return
	}
	slog.Info("claw: upgrade already installed on the device, marked succeeded",
		"machine", machineID, "version", currentVersion, "generation", upgrade.DesiredGeneration)
}

// clawDeviceUpgradeIntent 决定本轮要不要给这台机器下发升级指令。
//
// 三条拒绝线，全部返回 nil（「这次什么都别做」，与轮询失败同义 —— 绝不因为一次
// 读失败就让设备去下载 88MB）：
//  1. 没有行 / 状态不是待执行（pending|waiting_idle）；
//  2. 平台不是 Windows-x64（本期只支持它，服务端先拦一道，设备也会自己报
//     platform_unsupported）；
//  3. 同批次更早的波次还没跑完。
func (h *Handler) clawDeviceUpgradeIntent(
	ctx context.Context,
	machineNode db.RongcloudNode,
	currentVersion string,
) (*clawDeviceUpgradeIntent, error) {
	machineID := machineIDForDevice(machineNode)
	upgrade, found, err := h.clawNodeUpgradeByMachine(ctx, machineID)
	if err != nil || !found {
		return nil, err
	}
	if clawUpgradeTerminal(upgrade.Status) || upgrade.Status == "" {
		return nil, nil
	}
	if !clawUpgradePlatformSupported(upgrade.ReportedPlatform) {
		// 平台不支持 / 从没上报过平台：**必须**把这一行收尾，否则它永远停在
		// pending，既是页面上一条骗人的「排队中」，又会把同批次后面的波次堵死
		// （审查报告 S3）。这里直接落 failed 并给出人能看懂的原因 —— 设备侧本来
		// 也不会领（它自己也会报 platform_unsupported）。
		h.clawDeviceUpgradeFailPlatform(ctx, machineID, upgrade)
		return nil, nil
	}
	// 目标版本已经等于当前版本：设备上一轮其实装上了（比如重启后还没来得及回报
	// succeeded 就断电、或者账本写坏导致那次 succeeded 根本没发出去）。别让它再下
	// 一次 88MB，但**也不能什么都不做** —— 什么都不做的话这一行会永远停在
	// downloading/restarting，且不会自愈（审查报告 R5）。服务端在这里自己收尾：
	// 三个事实（目标版本、当前版本、当前状态）都在本次请求里，不需要读库比对。
	if upgrade.TargetVersion != "" && currentVersion == upgrade.TargetVersion {
		h.clawDeviceUpgradeCompleteInPlace(ctx, machineID, upgrade, currentVersion)
		return nil, nil
	}
	pending, err := h.clawDeviceUpgradePending(ctx, upgrade.BatchID, upgrade.Wave)
	if err != nil || !upgradeWaveOpen(pending) {
		return nil, err
	}
	release, _, err := resolveClawUpgradeLatest(ctx)
	if err != nil {
		// 服务端查不到 latest（无外网）：指令先挂着，下一轮再试。这里不落 failed ——
		// 「服务端暂时查不到最新版」不是设备的问题，也不该让管理员看到设备失败。
		slog.Warn("claw: upgrade intent deferred, latest release unavailable",
			"machine", machineID, "error", err)
		return nil, nil
	}
	if upgrade.TargetVersion != release.Version {
		// 请求发出后 latest 已经前进：缓存里只有**当前 latest** 的校验和，我们
		// 没有目标版本的可信摘要 —— 拒绝下发，而不是让设备装一个没法校验的 exe。
		// 管理员再点一次升级即可跟上新的 latest（页面会显示新目标版本）。
		slog.Warn("claw: upgrade target is no longer the latest release",
			"machine", machineID, "target", upgrade.TargetVersion, "latest", release.Version)
		return nil, nil
	}
	// 资产按**这台机器上报的平台**取：同一次批量下发里可能同时有 Windows 和
	// Linux，各自的文件名与摘要都不同，用同一个常量会让非 Windows 那批 404。
	asset := clawUpgradeAssetForPlatform(upgrade.ReportedPlatform)
	sum, ok := release.SHA256[asset]
	if asset == "" || !ok || len(sum) != 64 {
		slog.Warn("claw: release has no sha256 for the platform asset",
			"version", release.Version, "machine", machineID,
			"platform", upgrade.ReportedPlatform, "asset", asset)
		return nil, nil
	}
	token := signClawUpgradeToken(release.Version, asset, time.Now().Add(clawUpgradeTokenTTL))
	mirrors, direct := clawUpgradeMirrors(release, asset, token)
	if len(mirrors) == 0 {
		// 连直连地址都拼不出来（资产不在发布矩阵 / release 里没有这条资产）。
		// 下发出去只会让设备报一个看不懂的 download_failed，这里先按「本轮不下发」
		// 处理，等下一次 release 补齐资产。
		slog.Warn("claw: no download url for the platform asset",
			"version", release.Version, "machine", machineID, "asset", asset, "direct", direct)
		return nil, nil
	}
	return &clawDeviceUpgradeIntent{
		Generation:    upgrade.DesiredGeneration,
		TargetVersion: upgrade.TargetVersion,
		AssetName:     asset,
		SHA256:        sum,
		Force:         upgrade.Force,
		Mirrors:       mirrors,
	}, nil
}

// upgradeReportDecision 判断一条设备进度回传要不要落库。
//
//   - 代次不符：这条回传属于上一轮请求（管理员又点了一次 / 上一轮超时后重发），
//     丢掉，否则旧轮的 failed 会把新一轮的 downloading 覆盖掉；
//   - 非法状态：设备不该自己写 pending/”，那是服务端的领域；
//   - 已结束的行不接受非终态回传：succeeded 之后再来一条 downloading 说明设备
//     重放了旧日志，不能让它把页面拉回「下载中」。
func upgradeReportDecision(desiredGeneration, reportGeneration int64, currentStatus, newStatus string) (bool, string) {
	if reportGeneration != desiredGeneration || desiredGeneration == 0 {
		return false, "stale_generation"
	}
	if !clawUpgradeDeviceReportable[newStatus] {
		return false, "unknown_status"
	}
	if clawUpgradeTerminal(currentStatus) && !clawUpgradeTerminal(newStatus) {
		return false, "already_finished"
	}
	return true, ""
}

// ClawDeviceUpgradeReport POST /api/claw/device/upgrade/report —— 设备回传升级进度。
//
// 认证与 device/control 相同（机器节点 device credential）：升级是整机动作，
// 只有机器主节点能发起/汇报，agent 子进程（含 ops）不该能重启整台设备。
func (h *Handler) ClawDeviceUpgradeReport(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil || h.DB == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		NodeID           string `json:"nodeId"`
		CredentialID     string `json:"credentialId"`
		Secret           string `json:"secret"`
		Generation       int64  `json:"generation"`
		Status           string `json:"status"`
		ErrorCode        string `json:"errorCode"`
		ErrorDetail      string `json:"errorDetail"`
		ReportedVersion  string `json:"reportedVersion"`
		ReportedPlatform string `json:"reportedPlatform"`
		ReportedHostname string `json:"reportedHostname"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	ctx := r.Context()
	machineNode, err := h.RongCloudNode.AuthorizeDevice(ctx, req.NodeID, req.CredentialID, req.Secret)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: upgrade report auth failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "校验设备凭据失败", nil)
		return
	}
	machineID := machineIDForDevice(machineNode)
	if req.ReportedVersion != "" || req.ReportedPlatform != "" || req.ReportedHostname != "" {
		if err := h.clawDeviceRecordUpgradeState(ctx, machineNode, req.ReportedVersion, req.ReportedPlatform, req.ReportedHostname, req.Generation); err != nil {
			slog.Warn("claw: upgrade version report failed", "machine", machineID, "error", err)
		}
	}
	upgrade, found, err := h.clawNodeUpgradeByMachine(ctx, machineID)
	if err != nil {
		clawJSON(w, 500, 500, "查询升级状态失败", nil)
		return
	}
	if !found {
		clawJSON(w, 404, 404, "本机没有升级任务", nil)
		return
	}
	status := strings.TrimSpace(req.Status)
	apply, reason := upgradeReportDecision(upgrade.DesiredGeneration, req.Generation, upgrade.Status, status)
	if !apply {
		// 409 而不是 400：设备没做错什么，是这条回传过期了。设备拿到 409 只需
		// 忽略（下一轮轮询会拿到新的代次）。
		clawJSON(w, 409, 409, "回传已过期："+reason, map[string]interface{}{
			"applied": false, "reason": reason, "desired_generation": upgrade.DesiredGeneration,
		})
		return
	}
	errorCode := strings.TrimSpace(req.ErrorCode)
	errorDetail := strings.TrimSpace(req.ErrorDetail)
	if len(errorDetail) > 1000 {
		errorDetail = errorDetail[:1000]
	}
	if _, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_node_upgrade
		   SET status = $2, error_code = $3, error_detail = $4, updated_at = now()
		 WHERE machine_id = $1 AND desired_generation = $5`,
		machineID, status, errorCode, errorDetail, upgrade.DesiredGeneration); err != nil {
		slog.Warn("claw: upgrade report write failed", "machine", machineID, "error", err)
		clawJSON(w, 500, 500, "保存升级进度失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{
		"applied": true, "status": status, "generation": upgrade.DesiredGeneration,
	})
}
