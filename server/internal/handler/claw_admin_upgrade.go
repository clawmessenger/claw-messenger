package handler

// 管理端「下发设备升级」。
//
// 与启停同一套异步语义（claw_admin_nodes.go 的 start/stop）：这里只写**意图**，
// 真正下载/交换/重启的是设备上的 supervisor —— 它每 10 秒来 /api/claw/device/control
// 问一次，离线设备的指令就挂在表里等它上线（与「指令已下发，设备上线后执行」
// 完全同义）。
//
// 三条红线写在这个文件里：
//  1. 入参只有 machine 列表与 force 开关。版本、资产名、下载地址、摘要全部由
//     服务端解析（upgrade_release.go），前端递一个 URL 进来会被
//     DisallowUnknownFields 直接 400 —— 设备端也永远只接受「版本号」，不接受
//     URL，所以没有任何一条路径能把服务端变成任意地址的下载器。
//  2. 只升不降：目标版本永远是 latest。下发前如果设备当前版本已经 >= latest，
//     就跳过（页面提示「已是最新」），不做历史版本/回滚。
//  3. 批量按波次放行：一次请求可以覆盖整批机器，但同一时刻只有一波在下载
//     （默认 5 台）。88MB × 100 台同时下会把企业出口打满，而波次只需要服务端
//     算一个「更早的波次还有没有活的」判断，不需要后台调度器。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// clawUpgradeWaveSizeDefault 是每波放行的机器台数。
const clawUpgradeWaveSizeDefault = 5

// clawUpgradeMaxMachines 限制一次请求的机器数，避免一次点击写成一张巨大的表 +
// 一条巨大的审计。真要一次推 100 台以上，分两次点即可（波次语义是一样的）。
const clawUpgradeMaxMachines = 100

// clawAdminMachineKeyExpr 是「机器键」的 SQL 表达式，必须与 machineIDForDevice
// **逐字等价**（machine_id 优先，否则 rongcloud_user_id 去掉 `rc_node_` 前缀）。
// 两处推导不一致的后果很隐蔽：设备把版本写进 A 键、页面拿 B 键去查，版本列永远
// 是「未知」，而服务端一切正常（审查报告 M6）。
const clawAdminMachineKeyExpr = `COALESCE(NULLIF(machine_id, ''),
	CASE WHEN rongcloud_user_id LIKE 'rc_node_%' THEN substr(rongcloud_user_id, 9)
	     ELSE rongcloud_user_id END)`

// clawUpgradeWaveSize 允许用环境变量调整波次大小（运维按出口带宽调），
// 但**不是**硬编码在 handler 里：这个数属于部署策略，不属于一次请求。
func clawUpgradeWaveSize() int {
	if raw := strings.TrimSpace(os.Getenv("CLAW_UPGRADE_WAVE_SIZE")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return clawUpgradeWaveSizeDefault
}

// clawUpgradeTarget 是一台待升级机器的编排结果。
type clawUpgradeTarget struct {
	MachineID        string
	WorkspaceID      string
	Wave             int
	ReportedVersion  string
	ReportedPlatform string
}

// clawUpgradeSkip 是被跳过的机器及原因（原样回给页面，别让管理员猜）。
type clawUpgradeSkip struct {
	MachineID string `json:"machine_id"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"`
}

// planClawUpgradeWaves 按 waveSize 分批，返回带波次号的机器列表。纯函数：
// 「第几台进第几波」这种规则必须能被单测钉住，否则改个 +1/-1 会在生产上表现成
// 「第一批就全量下载」。
func planClawUpgradeWaves(machineIDs []string, waveSize int) []clawUpgradeTarget {
	if waveSize < 1 {
		waveSize = 1
	}
	out := make([]clawUpgradeTarget, 0, len(machineIDs))
	for i, machineID := range machineIDs {
		out = append(out, clawUpgradeTarget{
			MachineID: machineID,
			Wave:      i/waveSize + 1,
		})
	}
	return out
}

// dedupeClawUpgradeMachines 去掉空值与重复。同机多节点在页面上是多行，批量勾选
// 很容易把一台机器勾进来两次 —— 那会变成同一台机器下两次 88MB。
func dedupeClawUpgradeMachines(machineIDs []string) []string {
	seen := make(map[string]bool, len(machineIDs))
	out := make([]string, 0, len(machineIDs))
	for _, raw := range machineIDs {
		machineID := strings.TrimSpace(raw)
		if machineID == "" || seen[machineID] {
			continue
		}
		seen[machineID] = true
		out = append(out, machineID)
	}
	return out
}

// clawAdminUpgradeProgress 是进度接口/列表扩展的 JSON 形状（前端 UpgradeModal 用）。
//
// `RequestedAt` 与 `UpdatedAt` 的区别必须分清：`updated_at` 会被设备每 10 秒一次的
// 状态上报（`clawDeviceRecordUpgradeState` 里的 `updated_at = now()`）和心跳刷新，
// 它表示「最后一次进度变化」，**不能**用来算「已经等了多久」—— 拿它算的话等待时长
// 永远是 0（审查报告 R1）。`requested_at` 只在 `clawAdminUpsertUpgradeRequest` 的
// `ON CONFLICT` 里跟着本次下发一起写，语义是「这一轮从什么时候开始」。
type clawAdminUpgradeProgress struct {
	MachineID        string `json:"machine_id"`
	ReportedVersion  string `json:"reported_version"`
	ReportedPlatform string `json:"reported_platform,omitempty"`
	TargetVersion    string `json:"target_version,omitempty"`
	Status           string `json:"status,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	ErrorDetail      string `json:"error_detail,omitempty"`
	Force            bool   `json:"force"`
	Wave             int    `json:"wave,omitempty"`
	BatchID          string `json:"batch_id,omitempty"`
	RequestedAt      string `json:"requested_at,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

func clawAdminUpgradeProgressFor(upgrade clawNodeUpgrade) clawAdminUpgradeProgress {
	progress := clawAdminUpgradeProgress{
		MachineID:        upgrade.MachineID,
		ReportedVersion:  upgrade.ReportedVersion,
		ReportedPlatform: upgrade.ReportedPlatform,
		TargetVersion:    upgrade.TargetVersion,
		Status:           upgrade.Status,
		ErrorCode:        upgrade.ErrorCode,
		ErrorDetail:      upgrade.ErrorDetail,
		Force:            upgrade.Force,
		Wave:             upgrade.Wave,
		BatchID:          upgrade.BatchID,
	}
	if upgrade.RequestedAt.Valid {
		progress.RequestedAt = upgrade.RequestedAt.Time.UTC().Format(time.RFC3339)
	}
	if !upgrade.UpdatedAt.IsZero() {
		progress.UpdatedAt = upgrade.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return progress
}

// clawAdminUpgradeRequestBody 是升级下发唯一接受的入参。
type clawAdminUpgradeRequestBody struct {
	MachineIDs []string `json:"machine_ids"`
	Force      bool     `json:"force"`
}

// decodeClawUpgradeRequest 严格解码请求体：未知字段一律拒绝。
//
// 这就是目标版本白名单在入参侧的全部落点：`{"url": ...}` / `{"version": ...}` /
// `{"mirror": ...}` 这类请求必须在**解析阶段**就 400，而不是「解析后忽略」——
// 后者会让调用方以为它成功了。前端只被允许表达「这几台机器，强制或否」。
func decodeClawUpgradeRequest(r *http.Request) (clawAdminUpgradeRequestBody, error) {
	var body clawAdminUpgradeRequestBody
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return clawAdminUpgradeRequestBody{}, err
	}
	return body, nil
}

// ClawAdminRequestNodeUpgrade POST /api/admin/nodes/upgrade
//
// body: {"machine_ids": ["3300776275", ...], "force": false}
// 未知字段一律 400（白名单的落点：`{"url": "..."}` 这种请求必须当场被拒）。
func (h *Handler) ClawAdminRequestNodeUpgrade(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("node:write", w, r)
	if ident == nil {
		return
	}
	if h.DB == nil {
		clawAdminFail(w, 503, "数据库不可用")
		return
	}
	body, err := decodeClawUpgradeRequest(r)
	if err != nil {
		clawAdminFail(w, 400, "请求参数错误：只接受 machine_ids 与 force")
		return
	}
	machineIDs := dedupeClawUpgradeMachines(body.MachineIDs)
	if len(machineIDs) == 0 {
		clawAdminFail(w, 400, "请至少选择一台设备")
		return
	}
	if len(machineIDs) > clawUpgradeMaxMachines {
		clawAdminFail(w, 400, fmt.Sprintf("一次最多下发 %d 台设备", clawUpgradeMaxMachines))
		return
	}
	ctx := r.Context()

	// 目标版本 = 服务端解析出的 latest。解析不出来就明确报错（不假装成功）：
	// 自托管部署没有外网时，管理员应当看到「无法获取最新版本」，而不是点完
	// 按钮后设备永远 pending。
	release, stale, err := resolveClawUpgradeLatest(ctx)
	if err != nil {
		slog.Warn("claw admin: upgrade release lookup failed", "error", err)
		clawAdminFail(w, 503, err.Error())
		return
	}
	// 摘要校验下沉到 per-target：一次批量里可能同时有 Windows 与 Linux，两者对应
	// 不同资产名与不同 sha256，只有按目标机器自己上报的平台取才能真校验。
	targets, skips, err := h.clawAdminResolveUpgradeTargets(ctx, machineIDs, release.Version, release.SHA256)
	if err != nil {
		clawAdminFail(w, 500, "查询设备状态失败")
		return
	}
	if len(targets) == 0 {
		// 别只说「都不需要升级」：原因可能是平台未知、平台不支持、已是最新，三件
		// 事的处置完全不同，管理员得知道是哪一件。
		//
		// 同一件事也要进日志：页面上那句 400 转瞬即逝（关掉弹窗就没了），而「某个
		// 版本的所有平台都缺校验和」这类问题得能事后回答「哪个版本、多少台、为什么」。
		// 响应体这轮不加 skipped[]，前端契约不动 —— 只是把已有信息抄一份到日志。
		//
		// summary 是按原因聚合的（「该平台资产缺少校验和 1 台」），只有数量、不含
		// 资产名。asset_missing 的 Detail 本来就写着版本与资产名，取第一条抄进同一
		// 条日志，运维才答得出「缺的是哪个资产的校验和」；没有就留空串，不额外打日志。
		firstAssetMissingDetail := ""
		for _, skip := range skips {
			if skip.Reason == "asset_missing" && skip.Detail != "" {
				firstAssetMissingDetail = skip.Detail
				break
			}
		}
		slog.Warn("claw admin: 所选设备全部跳过，未下发任何升级",
			"target_version", release.Version,
			"requested", len(machineIDs),
			"skipped", len(skips),
			"summary", clawUpgradeSkipSummary(skips),
			"asset_detail", firstAssetMissingDetail,
			"stale_release", stale)
		clawAdminFail(w, 400, "所选设备都不能升级："+clawUpgradeSkipSummary(skips))
		return
	}

	batchID := fmt.Sprintf("upg-%d", time.Now().UnixNano())
	planned := planClawUpgradeWaves(machineIDsOf(targets), clawUpgradeWaveSize())
	// planClawUpgradeWaves 只关心顺序，这里把真实的机器属性（workspace/版本）
	// 按 machine_id 合回去。
	byMachine := make(map[string]clawUpgradeTarget, len(targets))
	for _, target := range targets {
		byMachine[target.MachineID] = target
	}
	issued := make([]clawAdminUpgradeProgress, 0, len(planned))
	// 本批各平台实际用到的摘要：同一次下发可能混合 Windows 与 Linux，
	// 它们下载的是不同文件，摘要也就不是同一个值。
	issuedSHA := make(map[string]string, 2)
	for _, item := range planned {
		target := byMachine[item.MachineID]
		target.Wave = item.Wave
		version, err := h.clawAdminUpsertUpgradeRequest(ctx, target, release.Version, batchID, ident.AdminID, body.Force)
		if err != nil {
			slog.Warn("claw admin: upgrade request write failed", "machine", target.MachineID, "error", err)
			skips = append(skips, clawUpgradeSkip{
				MachineID: target.MachineID, Reason: "write_failed", Detail: "写入升级任务失败",
			})
			continue
		}
		// 每台一条审计：批量升级要能回答「谁在什么时候把哪台机器升到了哪个版本」。
		h.clawAdminAudit(r, ident, "node_upgrade_requested", "node", target.MachineID, map[string]interface{}{
			"batch_id":       batchID,
			"target_version": release.Version,
			"wave":           target.Wave,
			"force":          body.Force,
			"generation":     version,
			"stale_release":  stale,
		})
		issued = append(issued, clawAdminUpgradeProgress{
			MachineID:        target.MachineID,
			ReportedVersion:  target.ReportedVersion,
			ReportedPlatform: target.ReportedPlatform,
			TargetVersion:    release.Version,
			Status:           clawUpgradeStatusPending,
			Force:            body.Force,
			Wave:             target.Wave,
			BatchID:          batchID,
		})
		if asset := clawUpgradeAssetForPlatform(target.ReportedPlatform); asset != "" {
			issuedSHA[asset] = release.SHA256[asset]
		}
	}
	// 顶层 sha256 只在「本批只有一个平台」时有意义（历史字段，前端已不读）；
	// 混平台时留空，真正的值按资产放在 sha256_by_asset 里。
	singleSHA := ""
	if len(issuedSHA) == 1 {
		for _, digest := range issuedSHA {
			singleSHA = digest
		}
	}
	// nil 切片必须换成空切片再出响应：`skips` 是命名返回值，一次「全部下发成功」
	// 的批量里它从没 append 过，`encoding/json` 会把它写成 `"skipped": null`，
	// 而管理端 UpgradeModal 读的是 `result.skipped.length` —— 线上就是这么白屏的
	// （TypeError: Cannot read properties of null (reading 'length')，整页被 React
	// 卸载）。契约上「没有跳过」是 []，不是 null。
	issued = clawAdminUpgradeNonNil(issued)
	skips = clawAdminUpgradeNonNil(skips)
	clawAdminOK(w, map[string]interface{}{
		"batch_id":        batchID,
		"target_version":  release.Version,
		"sha256":          singleSHA,
		"sha256_by_asset": issuedSHA,
		"stale_release":   stale,
		"wave_size":       clawUpgradeWaveSize(),
		// 服务端下载代理是否可用。没配 MULTICA_CLAW_UPGRADE_TOKEN_SECRET 时它是
		// false，mirrors 里就只剩直连 GitHub —— 设备出口打不通时全部 download_failed，
		// 而 error_detail 只有 GitHub 的报错。那 503 在「忘配 secret」的场景里**永远
		// 不会被打出来**（没配 → 根本不给设备代理地址 → 设备不会来请求），所以必须
		// 在**下发这一刻**就把这个事实告诉页面（审查报告 M2）。
		"proxy_available": clawUpgradeTokenSecretConfigured(),
		"issued":          issued,
		"skipped":         skips,
	})
}

// clawAdminUpgradeNonNil 把 nil 切片换成空切片，只用于出响应。
//
// 这不是洁癖，是线上事故：Go 的 nil slice 序列化成 JSON `null`，而管理端页面
// 直接读 `result.skipped.length` / `result.issued.map`。`skips` 在
// clawAdminResolveUpgradeTargets 里是命名返回值，一次「全部下发成功」的批量里
// 它从没 append 过、于是是 nil —— 响应里就是 `"skipped": null`，页面渲染当场
// TypeError（Cannot read properties of null (reading 'length')），React 卸载整棵树，
// 管理端白屏。契约上「没有跳过」是 []，「字段不存在」才是 null，两者不能混。
//
// 归一只做在边界（写响应之前），内部逻辑仍然可以自由用 nil 表示空集合。
func clawAdminUpgradeNonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

// clawUpgradeSkipSummary 把跳过原因压成一句人话（按原因计数，最多列 3 类）。
// 纯函数，便于单测。
func clawUpgradeSkipSummary(skips []clawUpgradeSkip) string {
	labels := map[string]string{
		"unknown_machine":      "服务端没有节点记录",
		"platform_unsupported": "平台不支持自动升级",
		"platform_unknown":     "还没上报 exe 平台（旧版本）",
		"asset_missing":        "该平台资产缺少校验和",
		"already_current":      "已是最新版本",
		"write_failed":         "写入升级任务失败",
	}
	order := []string{"platform_unknown", "platform_unsupported", "asset_missing", "already_current", "unknown_machine", "write_failed"}
	counts := make(map[string]int, len(order))
	for _, skip := range skips {
		counts[skip.Reason]++
	}
	// 已知原因按上面的顺序排；没见过的原因追加在后面（按名字排序，保证输出稳定），
	// 一个都不吞 —— 「另一种原因导致全部跳过」时页面更该看见它。
	reasons := make([]string, 0, len(counts))
	for _, reason := range order {
		if counts[reason] > 0 {
			reasons = append(reasons, reason)
		}
	}
	extra := make([]string, 0, 2)
	for reason := range counts {
		known := false
		for _, listed := range order {
			if reason == listed {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, reason)
		}
	}
	sort.Strings(extra)
	reasons = append(reasons, extra...)
	parts := make([]string, 0, 3)
	for _, reason := range reasons {
		label := labels[reason]
		if label == "" {
			label = reason
		}
		parts = append(parts, fmt.Sprintf("%s %d 台", label, counts[reason]))
		if len(parts) == 3 {
			break
		}
	}
	if len(parts) == 0 {
		return "没有可下发的设备"
	}
	return strings.Join(parts, "、")
}

func machineIDsOf(targets []clawUpgradeTarget) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.MachineID)
	}
	return out
}

// clawAdminResolveUpgradeTargets 把「管理员勾的机器」过滤成「真的要下发的机器」，
// 并把被跳过的那部分连同人话原因一起回给页面。
//
// 判定顺序是有意为之：先「这台机器存在吗」，再「平台支持吗」，最后「版本已经
// 够新吗」—— 否则一台非 Windows 设备会先因为「已是最新」被跳过，管理员就看不到
// 「平台不支持」这条真正的原因。
func (h *Handler) clawAdminResolveUpgradeTargets(
	ctx context.Context,
	machineIDs []string,
	targetVersion string,
	sha256ByAsset map[string]string,
) (targets []clawUpgradeTarget, skips []clawUpgradeSkip, err error) {
	workspaces, err := h.clawAdminMachineWorkspaces(ctx, machineIDs)
	if err != nil {
		return nil, nil, err
	}
	upgrades, err := h.clawAdminUpgradesForMachines(ctx, machineIDs)
	if err != nil {
		return nil, nil, err
	}
	for _, machineID := range machineIDs {
		workspaceID, known := workspaces[machineID]
		if !known {
			skips = append(skips, clawUpgradeSkip{
				MachineID: machineID, Reason: "unknown_machine", Detail: "服务端没有这台机器的节点记录",
			})
			continue
		}
		upgrade, hasUpgrade := upgrades[machineID]
		if !hasUpgrade || upgrade.ReportedPlatform == "" {
			// 从没上报过平台：可能是刚装还没轮询过，也可能是 1.3.0 之前的老版本。
			// 不下发 —— 下发出去也只会变成一条「设备永远领不走」的行，还会连带
			// 把波次闸堵死（审查报告 S3）。页面拿到的是当场就能读懂的原因。
			skips = append(skips, clawUpgradeSkip{
				MachineID: machineID, Reason: "platform_unknown",
				Detail: "设备还没有上报 exe 平台（1.3.0 之前的版本不会上报），请先在设备上安装一次",
			})
			continue
		}
		if !clawUpgradePlatformSupported(upgrade.ReportedPlatform) {
			skips = append(skips, clawUpgradeSkip{
				MachineID: machineID, Reason: "platform_unsupported",
				Detail: fmt.Sprintf("支持的是 %s，本机上报的是 %s", clawUpgradeSupportedPlatformsText(), upgrade.ReportedPlatform),
			})
			continue
		}
		// 摘要必须按这台机器要下载的那个资产去取（Windows 与 Linux 是不同文件）。
		// 缺摘要就不下发：宁可当场说「这个版本的校验和还没齐」，也不能让设备装
		// 一个无法校验的二进制 —— 校验失败那一步在设备侧是不可恢复的。
		//
		// 摘要有两个来源（SHA256SUMS 优先，Releases API 的 digest 兜底，见
		// upgrade_release.go），所以文案不再点名 SHA256SUMS —— 两条都拿不到时
		// 说「SUMS 里没有」会把运维指向一个错的地方。
		asset := clawUpgradeAssetForPlatform(upgrade.ReportedPlatform)
		if sum := sha256ByAsset[asset]; len(sum) != 64 {
			skips = append(skips, clawUpgradeSkip{
				MachineID: machineID, Reason: "asset_missing",
				Detail: fmt.Sprintf("版本 %s 的 %s 拿不到校验和（SHA256SUMS 与 Release 资产 digest 都没有）", targetVersion, asset),
			})
			continue
		}
		if upgrade.ReportedVersion != "" &&
			compareClawVersions(upgrade.ReportedVersion, targetVersion) >= 0 {
			skips = append(skips, clawUpgradeSkip{
				MachineID: machineID, Reason: "already_current",
				Detail: fmt.Sprintf("当前已是 %s", upgrade.ReportedVersion),
			})
			continue
		}
		target := clawUpgradeTarget{
			MachineID:        machineID,
			WorkspaceID:      workspaceID,
			ReportedVersion:  upgrade.ReportedVersion,
			ReportedPlatform: upgrade.ReportedPlatform,
		}
		targets = append(targets, target)
	}
	return targets, skips, nil
}

// compareClawVersions 比较 x.y.z，忽略预发布后缀。返回 -1/0/1。
// 与 CLI 的 compareVersions 同语义：只升不降的判据必须两边一致，否则服务端
// 认为「已是最新」而设备还在旧版本上，升级就永远发不下去。
func compareClawVersions(a, b string) int {
	split := func(v string) []int {
		head := strings.SplitN(v, "-", 2)[0]
		parts := strings.Split(head, ".")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			n, _ := strconv.Atoi(strings.TrimSpace(part))
			out = append(out, n)
		}
		return out
	}
	pa, pb := split(a), split(b)
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// clawAdminMachineWorkspaces 按 machine_id 取 workspace：升级行需要它，而设备
// 可能还没轮询过（那时 upgrade 行还不存在）。
//
// 机器键的推导必须和 device 侧逐字一致（machineIDForDevice）：machine_id 为空
// 的老行用融云 id 剥掉 `rc_node_` 前缀兜底，否则这类机器在页面上永远显示「未知
// 版本」、升级按钮永远禁用（审查报告 M6）。
//
// `min(workspace_id::text)` 而不是 `min(workspace_id)`：PostgreSQL 的 min/max
// 聚合**没有 uuid 版本**，聚合内部传 uuid 会直接 `function min(uuid) does not
// exist`，整个下发接口 500（审查报告 S5）。这里要的只是「随便取一个」，text 的
// min 一定存在。
func (h *Handler) clawAdminMachineWorkspaces(ctx context.Context, machineIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(machineIDs))
	rows, err := h.DB.Query(ctx, `
		SELECT `+clawAdminMachineKeyExpr+`, min(workspace_id::text)
		  FROM rongcloud_node
		 WHERE `+clawAdminMachineKeyExpr+` = ANY($1)
		 GROUP BY 1`, machineIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var machineID, workspaceID string
		if err := rows.Scan(&machineID, &workspaceID); err != nil {
			return nil, err
		}
		out[machineID] = workspaceID
	}
	return out, rows.Err()
}

// clawAdminUpgradesForMachines 批量读升级行（一次查询，不做 N+1）。
func (h *Handler) clawAdminUpgradesForMachines(ctx context.Context, machineIDs []string) (map[string]clawNodeUpgrade, error) {
	out := make(map[string]clawNodeUpgrade, len(machineIDs))
	rows, err := h.DB.Query(ctx, `SELECT `+clawNodeUpgradeColumns+`
		  FROM rongcloud_node_upgrade WHERE machine_id = ANY($1)`, machineIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		upgrade, err := scanClawNodeUpgrade(rows)
		if err != nil {
			return nil, err
		}
		out[upgrade.MachineID] = upgrade
	}
	return out, rows.Err()
}

// clawAdminUpsertUpgradeRequest 写入一次升级意图，返回推进后的代次。
// 与 rongcloud_node_control 的 desired_version 同一模型：每次下发都推进，
// 设备据此区分「同一条指令」与「又点了一次」。
func (h *Handler) clawAdminUpsertUpgradeRequest(
	ctx context.Context,
	target clawUpgradeTarget,
	targetVersion, batchID, actor string,
	force bool,
) (int64, error) {
	var generation int64
	err := h.DB.QueryRow(ctx, `
		INSERT INTO rongcloud_node_upgrade
			(machine_id, workspace_id, target_version, desired_generation, status,
			 force, batch_id, wave, requested_by, requested_at, updated_at)
		VALUES ($1, $2::uuid, $3, 1, $4, $5, $6, $7, $8, now(), now())
		ON CONFLICT (machine_id) DO UPDATE
		   SET workspace_id = EXCLUDED.workspace_id,
		       target_version = EXCLUDED.target_version,
		       desired_generation = rongcloud_node_upgrade.desired_generation + 1,
		       status = EXCLUDED.status,
		       error_code = '',
		       error_detail = '',
		       force = EXCLUDED.force,
		       batch_id = EXCLUDED.batch_id,
		       wave = EXCLUDED.wave,
		       requested_by = EXCLUDED.requested_by,
		       requested_at = now(),
		       updated_at = now()
		RETURNING desired_generation`,
		target.MachineID, target.WorkspaceID, targetVersion, clawUpgradeStatusPending,
		force, batchID, target.Wave, actor).Scan(&generation)
	return generation, err
}

// ClawAdminGetNodeUpgrade GET /api/admin/nodes/upgrade/{machine_id}
//
// 进度查询按 machine_id（不是 node_id）：一台机器上可能有多个 agent 节点，
// 它们共享同一个 exe，也就共享同一个升级任务。
func (h *Handler) ClawAdminGetNodeUpgrade(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	if h.DB == nil {
		clawAdminFail(w, 503, "数据库不可用")
		return
	}
	machineID := strings.TrimSpace(chi.URLParam(r, "machine_id"))
	if machineID == "" {
		clawAdminFail(w, 400, "缺少机器标识")
		return
	}
	// 读库之前先把「动作中失联」的行收尾：页面上那行不能永远转圈（见
	// clawUpgradeSweepStalled 的注释）。sweep 失败只记日志 —— 查询本身不该因为
	// 一次收尾写失败而 500，管理员至少还能看到当前状态。
	if err := h.clawUpgradeSweepStalled(r.Context(), "", machineID); err != nil {
		slog.Warn("claw admin: failed to sweep a stalled upgrade",
			"machine", machineID, "error", err)
	}
	upgrade, found, err := h.clawNodeUpgradeByMachine(r.Context(), machineID)
	if err != nil {
		clawAdminFail(w, 500, "查询升级进度失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "该机器没有升级记录")
		return
	}
	clawAdminOK(w, clawAdminUpgradeProgressFor(upgrade))
}

// ClawAdminListNodeUpgrades GET /api/admin/nodes/upgrade?batch_id=upg-...
//
// 一次批量升级的整批进度。为什么要这个接口：弹窗确认后是每 4 秒刷一次，如果按
// 机器逐台查，100 台就是 25 req/s、1500 req/min（审查报告 M4）；而 `(batch_id,
// wave)` 索引本来就在（迁移 601），一次查询天然更便宜，也避免「其中一台请求失败
// 就永远停在旧状态」。
func (h *Handler) ClawAdminListNodeUpgrades(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	if h.DB == nil {
		clawAdminFail(w, 503, "数据库不可用")
		return
	}
	batchID := strings.TrimSpace(r.URL.Query().Get("batch_id"))
	if batchID == "" {
		clawAdminFail(w, 400, "缺少批次标识")
		return
	}
	// 同单台查询：先收尾失联行再读。这里的弹窗每 4 秒刷一次，所以「设备掉线后
	// 15 分钟内自动变成可读懂的失败」不需要任何后台定时器。
	if err := h.clawUpgradeSweepStalled(r.Context(), batchID, ""); err != nil {
		slog.Warn("claw admin: failed to sweep stalled upgrades in a batch",
			"batch", batchID, "error", err)
	}
	rows, err := h.DB.Query(r.Context(), `SELECT `+clawNodeUpgradeColumns+`
		  FROM rongcloud_node_upgrade WHERE batch_id = $1
		 ORDER BY wave, machine_id`, batchID)
	if err != nil {
		clawAdminFail(w, 500, "查询升级进度失败")
		return
	}
	defer rows.Close()
	items := make([]clawAdminUpgradeProgress, 0, 8)
	for rows.Next() {
		upgrade, err := scanClawNodeUpgrade(rows)
		if err != nil {
			clawAdminFail(w, 500, "查询升级进度失败")
			return
		}
		items = append(items, clawAdminUpgradeProgressFor(upgrade))
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 500, "查询升级进度失败")
		return
	}
	clawAdminOK(w, map[string]interface{}{"batch_id": batchID, "items": items})
}
