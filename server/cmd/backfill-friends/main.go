// backfill-friends repairs the RongCloud friend edges for devices that were
// bound before the bind flow started writing them, and can re-sync node
// nicknames to RongCloud (-sync-names) for devices renamed before the rename
// flow started pushing to RongCloud.
//
// It can also unify default portraits (-sync-portraits): every account whose
// portrait is empty or exactly one of the known legacy default images is moved
// to the single platform default (rongcloud.DefaultUserPortrait). Custom
// portraits are never touched — the match is exact, never a LIKE pattern.
// Coverage: rongcloud_user + claw_im_users (pushed to RongCloud, because
// getToken never updates an existing user), rongcloud_group (pushed via
// /group/refresh.json — group/create takes no portrait; target is the 群聊
// default image, not the human one), and the local-only portrait columns of
// rongcloud_chatroom (target picked from conversation_kind: discussion /
// chatroom) and rongcloud_chatroom_member (unified default; the RongCloud
// chatroom API has no portrait field at all).
//
// The web client's 好友列表 (contact list) reads the RongCloud friend list, so a
// device bound before BindAgents called /friend/add.json never showed up there.
// This command walks the visible agent nodes of a workspace (the same nodes the
// device list shows: the per-machine infrastructure node and the internal ops
// node are skipped) and friends each one to every target claw/IM user.
//
// It is safe to rerun: RongCloud answers 25460 for an existing edge, which is
// reported as "already" and counts as success. Rows already carrying the
// unified default portrait are skipped, so a rerun costs no RongCloud calls.
// Nothing is written unless -commit is passed — the default is a dry run that
// only prints the plan.
//
// It can also repair the device->owner projection column (-claims):
// rongcloud_node.claimed_by_user_id is only written by the pairing-bind path and
// the admin transfer path, so devices bound before those existed (and devices
// bound through BindAgentsByDeviceCredential, which passes an empty owner) carry
// NULL. The friend list groups "人和他的智能体" by that column, so a NULL owner
// makes every agent render flat next to its human. -claims rebuilds the column
// from the authoritative source — the RongCloud friend edges — and writes a node
// only when the owner is uniquely determinable (exactly one account has the node
// as a friend, or the intersection over one machine_id is exactly one account).
// Ambiguous nodes are deliberately left empty rather than guessed.
//
// Typical use (from the server/ directory):
//
//	go run ./cmd/backfill-friends -env-file ../.env -commit -all-users
//	go run ./cmd/backfill-friends -env-file ../.env -commit -user 100123,100456
//	go run ./cmd/backfill-friends -env-file ../.env -commit -sync-names
//	go run ./cmd/backfill-friends -env-file ../.env -sync-portraits
//	go run ./cmd/backfill-friends -env-file ../.env -claims            # dry run
//	go run ./cmd/backfill-friends -env-file ../.env -claims -commit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"golang.org/x/sync/errgroup"
)

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("friend backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	userFlag := flag.String("user", "", "comma-separated claw_im_users.user_id values to backfill; also restricts -sync-portraits to these RongCloud user ids (pass rc_node_*/agent_* ids to target those rows)")
	allUsers := flag.Bool("all-users", false, "backfill every active claw_im_users account")
	workspaceFlag := flag.String("workspace", "", "workspace UUID to backfill (default: every workspace that has nodes)")
	commit := flag.Bool("commit", false, "write the friend edges (default is a dry run)")
	includeOps := flag.Bool("include-ops", false, "also friend the internal ops node (hidden from the friend list by default)")
	bidirectional := flag.Bool("both", false, "also write the reverse edge (agent -> owner)")
	syncNames := flag.Bool("sync-names", false, "push every node's database nickname to RongCloud (/user/refresh.json) instead of, or in addition to, friending")
	syncPortraits := flag.Bool("sync-portraits", false, "unify default portraits for claw_im_users and rongcloud_user, and switch group / chatroom default images to the per-kind group art (群聊 / 讨论组 / 聊天室); custom portraits are never overwritten")
	syncAgentPortraits := flag.Bool("sync-agent-portraits", false, "give every AI agent its own brand portrait (<base>/<kind>-256.png; base = $CLAW_AGENT_PORTRAIT_BASE or the claw-web static dir). Only rows whose RongCloud id resolves to an agent kind are touched; custom portraits are never overwritten. Mutually exclusive with -sync-portraits")
	registerMissing := flag.Bool("register-missing", false, "with -sync-agent-portraits: when RongCloud answers 1002 (userId not exist), create the account via /user/getToken.json (name + portraitUri in one call) instead of only reporting it as missing. Off by default: this writes identity, not just a field")
	claims := flag.Bool("claims", false, "backfill rongcloud_node.claimed_by_user_id from the RongCloud friend edges: writes a node only when its owner is uniquely determinable (one claimant, or the intersection over one machine_id is one account); ambiguous nodes stay empty (default is a dry run)")
	dsnFlag := flag.String("dsn", "", "postgres DSN (default: $DATABASE_URL, then $MULTICA_DATABASE_URL)")
	envFile := flag.String("env-file", "", "load KEY=VALUE environment variables from this file before reading configuration")
	flag.Parse()

	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	}

	if *userFlag == "" && !*allUsers && !*syncNames && !*syncPortraits && !*syncAgentPortraits && !*claims {
		return fmt.Errorf("specify -user <id[,id...]>, -all-users, -sync-names, -sync-portraits, or -claims to choose what to repair")
	}

	if *registerMissing && !*syncAgentPortraits {
		// 单独给 -register-missing 没有意义：补建只发生在品牌头像那一路。
		return fmt.Errorf("-register-missing 必须和 -sync-agent-portraits 一起用")
	}

	dsn := firstNonEmpty(*dsnFlag, os.Getenv("DATABASE_URL"), os.Getenv("MULTICA_DATABASE_URL"))
	if dsn == "" {
		return fmt.Errorf("no database DSN: pass -dsn or set DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	queries := db.New(pool)

	key, err := secretbox.LoadKey("MULTICA_RONGCLOUD_SECRET_KEY")
	if err != nil {
		return fmt.Errorf("load MULTICA_RONGCLOUD_SECRET_KEY: %w", err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("init secretbox: %w", err)
	}
	client := rongcloud.NewRongCloudAPIClientForServices(box, queries, slog.Default())
	svc := rongcloud.NewNodeService(queries, client, box, slog.Default())

	// -claims 只碰 rongcloud_node 一张表，与 workspace / claw 用户白名单无关，
	// 所以放在解析 workspace 之前：纯 -claims 调用不该因为「没有 workspace」
	// 或「没有 -user」而报错退出。
	if *claims {
		if err := runClaims(ctx, pool, client, *commit); err != nil {
			return fmt.Errorf("backfill node claims: %w", err)
		}
		if *userFlag == "" && !*allUsers && !*syncNames && !*syncPortraits && !*syncAgentPortraits {
			return nil
		}
		fmt.Println()
	}

	workspaces, err := resolveWorkspaces(ctx, pool, *workspaceFlag)
	if err != nil {
		return err
	}
	users, err := resolveUsers(ctx, pool, *userFlag, *allUsers)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no rongcloud workspace found to backfill")
	}
	if len(users) == 0 && !*syncNames && !*syncPortraits && !*syncAgentPortraits {
		return fmt.Errorf("no claw user matched; nothing to do")
	}

	opts := rongcloud.BackfillFriendsOptions{
		IncludeOps:    *includeOps,
		Bidirectional: *bidirectional,
		DryRun:        !*commit,
		// -user 同时约束 -sync-portraits 的 rongcloud_user 那一路（白名单）。
		OnlyRCUserIDs:   users,
		AgentPortraits:  *syncAgentPortraits,
		RegisterMissing: *syncAgentPortraits && *registerMissing,
	}
	if opts.DryRun {
		fmt.Println("DRY RUN — nothing will be written. Re-run with -commit to apply.")
	}
	fmt.Printf("workspaces=%d users=%d include_ops=%v bidirectional=%v sync_names=%v sync_portraits=%v sync_agent_portraits=%v register_missing=%v\n\n",
		len(workspaces), len(users), opts.IncludeOps, opts.Bidirectional, *syncNames, *syncPortraits, *syncAgentPortraits, *registerMissing)

	failed := 0

	if *syncNames {
		var renamed, refreshed, skipped int
		for _, ws := range workspaces {
			outcomes, err := svc.SyncNodeNicknames(ctx, ws, opts)
			if err != nil {
				return fmt.Errorf("sync nicknames workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				switch o.Status {
				case "failed":
					failed++
				case "renamed":
					renamed++
				case "refreshed":
					refreshed++
				case "skipped":
					skipped++
				}
				line := fmt.Sprintf("%-15s node=%-46s name=%s", o.Status, o.NodeRC, o.Name)
				if o.Error != "" {
					line += " error=" + o.Error
				}
				fmt.Println(line)
			}
		}
		fmt.Printf("nickname summary: renamed=%d refreshed=%d skipped=%d failed=%d\n", renamed, refreshed, skipped, failed)
		fmt.Println()
	}

	// agent 品牌头像：独立开关，且与「统一默认图」互斥 —— 两者折向同一批行，
	// 同时开等于自己覆盖自己。只碰 rongcloud_user（真人表 claw_im_users 不参与）。
	if *syncAgentPortraits {
		if *syncPortraits {
			return fmt.Errorf("-sync-portraits 与 -sync-agent-portraits 互斥：前者把默认图统一成一张，后者按 agent 类型发各自的品牌图")
		}
		var summary portraitSummary
		for _, ws := range workspaces {
			outcomes, err := svc.SyncUserPortraits(ctx, ws, opts)
			if err != nil {
				return fmt.Errorf("sync agent portraits workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				summary.add(o.Status)
				printPortrait(o.Scope, o.Status, o.RCUser, o.From, o.To, o.Error)
			}
		}
		failed += summary.failed
		fmt.Printf("agent portrait summary: planned=%d updated=%d registered=%d missing=%d skipped=%d failed=%d\n", summary.planned, summary.updated, summary.registered, summary.missing, summary.skipped, summary.failed)
		fmt.Println()
	}

	if *syncPortraits {
		var summary portraitSummary
		// rongcloud_user：AI agent / 机器主节点 / 真人镜像行。
		// -user 同时是这一路的白名单（opts.OnlyRCUserIDs），否则「只补某个用户」
		// 会顺手把每个 workspace 的 rongcloud_user 全刷一遍。
		for _, ws := range workspaces {
			outcomes, err := svc.SyncUserPortraits(ctx, ws, opts)
			if err != nil {
				return fmt.Errorf("sync portraits workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				summary.add(o.Status)
				printPortrait(o.Scope, o.Status, o.RCUser, o.From, o.To, o.Error)
			}
		}
		// claw_im_users：真人账号表（融云侧必须 refreshUser，SQL 改不到融云）。
		clawSummary, err := syncClawUserPortraits(ctx, pool, svc, users, opts.DryRun)
		if err != nil {
			return fmt.Errorf("sync claw user portraits: %w", err)
		}
		summary.merge(clawSummary)
		// -user 是严格白名单：群 / 聊天室 / 成员表没有用户维度（群是多人共享的，
		// 后两张表连 workspace_id 都没有），无法按用户裁剪，所以「只补某个用户」
		// 时不顺手改全局数据 —— 要补群/房间请单独跑一次不带 -user 的 -sync-portraits。
		if len(users) > 0 {
			fmt.Println("skip groups/chatrooms/members: -user 只补该用户（这些表没有用户维度）；需要时请单独跑不带 -user 的 -sync-portraits")
		} else {
			// rongcloud_group：群头像只能靠 /group/refresh.json 补写，先推融云再回写本地列。
			groupSummary, err := syncGroupPortraits(ctx, pool, svc, opts.DryRun)
			if err != nil {
				return fmt.Errorf("sync group portraits: %w", err)
			}
			summary.merge(groupSummary)
			// rongcloud_chatroom：按 conversation_kind 落讨论组 / 聊天室两种默认图
			// （融云聊天室 API 没有头像字段，只回写本地列）。
			chatroomSummary, err := syncChatroomPortraits(ctx, pool, opts.DryRun)
			if err != nil {
				return fmt.Errorf("sync chatroom portraits: %w", err)
			}
			summary.merge(chatroomSummary)
			// rongcloud_chatroom_member：成员是人和 AI 节点，继续落统一默认图。
			localSummary, err := syncLocalPortraitColumn(ctx, pool, "rongcloud_chatroom_member", "member_id", opts.DryRun)
			if err != nil {
				return fmt.Errorf("sync rongcloud_chatroom_member portraits: %w", err)
			}
			summary.merge(localSummary)
		}
		failed += summary.failed
		fmt.Printf("portrait summary: planned=%d updated=%d registered=%d missing=%d skipped=%d failed=%d\n", summary.planned, summary.updated, summary.registered, summary.missing, summary.skipped, summary.failed)
		fmt.Println()
	}

	if len(users) > 0 {
		var added, already, planned int
		for _, ws := range workspaces {
			outcomes, err := svc.BackfillAgentFriends(ctx, ws, users, opts)
			if err != nil {
				return fmt.Errorf("backfill workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				switch o.Status {
				case "added":
					added++
				case "already":
					already++
				case "failed":
					failed++
				case "planned":
					planned++
				}
				line := fmt.Sprintf("%-7s owner=%s agent=%-10s node=%s", o.Status, o.Owner, o.Agent, o.NodeRC)
				if o.Error != "" {
					line += " error=" + o.Error
				}
				fmt.Println(line)
			}
		}
		fmt.Printf("friends summary: added=%d already=%d failed=%d planned=%d\n", added, already, failed, planned)
	}

	if failed > 0 {
		return fmt.Errorf("%d operation(s) failed", failed)
	}
	return nil
}

// claimsFriendLister 只依赖 FriendIDs，调用方与测试都不必构造完整的融云客户端。
type claimsFriendLister interface {
	FriendIDs(ctx context.Context, userID string) ([]string, error)
}

// claimsLookupTimeout 是整轮好友边反查的总预算。这里是批量工具，不在「打开通讯录」
// 的热路径上，所以可以比热路径的 3s 宽松；但必须封顶 —— 候选账号上百个时，没有
// 总预算的批量外网调用会把运维钉在终端前。
const claimsLookupTimeout = 60 * time.Second

// claimsLookupConcurrency 与 friendNodeOwners.build 的反查并发（8）保持一致，
// 免得批量工具把融云的 per-app 频率配额打满，affected 到线上登录。
const claimsLookupConcurrency = 8

// nodeRow 是一行待判定节点（只取判定需要的列）。
type nodeRow struct {
	RCUserID  string // rongcloud_node.rongcloud_user_id
	MachineID string // rongcloud_node.machine_id
	AiType    string // rongcloud_node.ai_type
	Claimed   string // rongcloud_node.claimed_by_user_id；空 = 待填
}

// claimVerdict 是判定档位，只用于计划表与汇总。
type claimVerdict string

const (
	claimVerdictSingle    claimVerdict = "single"    // 单节点唯一 claimant
	claimVerdictMachine   claimVerdict = "machine"   // 机器级交集唯一
	claimVerdictAmbiguous claimVerdict = "ambiguous" // 多个候选，不写
	claimVerdictNoEdge    claimVerdict = "no-edge"   // 没有任何好友边，不写
	claimVerdictHidden    claimVerdict = "hidden"    // 机器主节点 / 运维节点，不在好友列表里
	claimVerdictAlready   claimVerdict = "already"   // 已有归属，不参与判定
)

// claimDecision 是一行节点的判定结果。Owner 非空当且仅当这一行可以安全写入。
type claimDecision struct {
	Node      nodeRow
	Owner     string
	Verdict   claimVerdict
	Claimants []string // 排序后的候选归属人，供计划表打印
}

// resolveClaims 是 -claims 的判定核心（纯函数，不碰库也不碰融云，便于单测）。
//
// claimants[nodeRCID] = 「把这个节点加为好友的账号集合」，即好友边反查的原始结果。
// 规则只有一条：**唯一解才写**，有歧义宁可不说 —— 归属写错会让一个智能体出现在
// 别人的「我的智能体」块里，比平铺更糟。
//
//	第一轮：claimants 恰好一个人 → 归他。
//	第二轮：按 machine_id 把同一台机器的节点分组求交集（同一台设备只可能有一个
//	        主人，第一轮已判定的节点也是证据）；交集恰好一个人 → 这台机器上所有
//	        待判节点都归他。
//	其余：no-edge（没有任何好友边）/ ambiguous（多解），都不写。
func resolveClaims(nodes []nodeRow, claimants map[string]map[string]bool) []claimDecision {
	decisions := make([]claimDecision, len(nodes))
	for i, n := range nodes {
		decisions[i] = claimDecision{Node: n, Claimants: sortedClaimKeys(claimants[n.RCUserID])}
	}

	// 第一轮：单节点唯一解。len(claimants)==1 时没有第二种解释。
	eligible := make([]int, 0, len(decisions))
	for i := range decisions {
		d := &decisions[i]
		switch {
		case strings.TrimSpace(d.Node.Claimed) != "":
			// 已有归属的行不参与判定，也不允许被覆盖（写库 SQL 同样带空值条件，
			// 两道保险）。
			d.Verdict = claimVerdictAlready
		case isHiddenClaimNode(d.Node):
			d.Verdict = claimVerdictHidden
		default:
			eligible = append(eligible, i)
			if len(d.Claimants) == 1 {
				d.Owner = d.Claimants[0]
				d.Verdict = claimVerdictSingle
			}
		}
	}

	// 第二轮：机器级交集。同一台设备只可能有一个主人，所以把同 machine_id 的
	// 节点求交。分组里包含第一轮已判定的节点 —— 它本身就是机器归属的证据：
	// A={u1,u2}、B={u1} 时 A 也只能归 u1。写只落在第一轮没判定的行上。
	// 没有 machine_id 的行无法归组，留给第三档。
	order := make([]string, 0, len(eligible))
	groups := make(map[string][]int, len(eligible))
	for _, i := range eligible {
		mid := strings.TrimSpace(decisions[i].Node.MachineID)
		if mid == "" {
			continue
		}
		if _, seen := groups[mid]; !seen {
			order = append(order, mid)
		}
		groups[mid] = append(groups[mid], i)
	}
	for _, mid := range order {
		idxs := groups[mid]
		common := intersectClaimants(idxs, claimants, decisions)
		if len(common) != 1 {
			continue
		}
		for _, i := range idxs {
			if decisions[i].Owner != "" {
				continue
			}
			decisions[i].Owner = common[0]
			decisions[i].Verdict = claimVerdictMachine
		}
	}

	// 剩下的仍无唯一解：自己一条好友边都没有 → no-edge，否则 → ambiguous。
	for _, i := range eligible {
		d := &decisions[i]
		if d.Owner != "" {
			continue
		}
		if len(d.Claimants) == 0 {
			d.Verdict = claimVerdictNoEdge
			continue
		}
		d.Verdict = claimVerdictAmbiguous
	}
	return decisions
}

// intersectClaimants 求一组节点 claimant 集合的交集，结果按字典序（报表稳定）。
func intersectClaimants(idxs []int, claimants map[string]map[string]bool, decisions []claimDecision) []string {
	if len(idxs) == 0 {
		return nil
	}
	first := claimants[decisions[idxs[0]].Node.RCUserID]
	common := make([]string, 0, len(first))
	for candidate := range first {
		inAll := true
		for _, i := range idxs[1:] {
			if !claimants[decisions[i].Node.RCUserID][candidate] {
				inAll = false
				break
			}
		}
		if inAll {
			common = append(common, candidate)
		}
	}
	sort.Strings(common)
	return common
}

// isHiddenClaimNode 与 rongcloud.isHiddenNode 同口径（运维 agent + 机器主节点）：
// 这两类节点不出现在好友列表里，填归属没有意义。机器主节点复用导出的
// rongcloud.IsMachineNode，避免在 Go 里重写 `rc_node_ || machine_id` 的归一化
// （MAC 大小写、.local 后缀）而与设备侧漂移。
func isHiddenClaimNode(r nodeRow) bool {
	if strings.TrimSpace(r.AiType) == rongcloud.OpsAgentType {
		return true
	}
	return rongcloud.IsMachineNode(db.RongcloudNode{
		RongcloudUserID: r.RCUserID,
		MachineID:       r.MachineID,
		AiType:          pgtype.Text{String: r.AiType, Valid: r.AiType != ""},
	})
}

// sortedClaimKeys 把集合摊平并按字典序排序。
func sortedClaimKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// claimFillSQL 只填空值：WHERE 里带 NULL / 空串条件，绝不覆盖已有归属。
// 判定与写入之间可能插入真实的绑定写路径（SetNodeClaim），那时 RowsAffected
// 会是 0 —— 调用方据此报告「已被抢先填写」，而不是报成功。
const claimFillSQL = `UPDATE rongcloud_node
	SET claimed_by_user_id = $1, updated_at = now()
	WHERE rongcloud_user_id = $2
	  AND (claimed_by_user_id IS NULL OR btrim(claimed_by_user_id) = '')`

// runClaims 是 -claims 的入口：反查好友边 → 判定 → （仅 -commit）写库。
//
// 幂等：已填的行不在待填集合里，重跑不会重复写，也不会覆盖别人的归属。
func runClaims(ctx context.Context, pool *pgxpool.Pool, lister claimsFriendLister, commit bool) error {
	if !commit {
		fmt.Println("DRY RUN — nothing will be written. Re-run with -commit to apply.")
	}
	candidates, err := resolveUsers(ctx, pool, "", true)
	if err != nil {
		return err
	}
	fmt.Printf("claim candidates (claw_im_users active): %d\n", len(candidates))

	claimants, lookupFailed := collectClaimants(ctx, lister, candidates)
	nodes, err := listClaimNodes(ctx, pool)
	if err != nil {
		return err
	}
	decisions := resolveClaims(nodes, claimants)

	var (
		pending, single, machine, ambiguous, noEdge, hidden, already int
		written, writeFailed, lostRace                               int
		stuck                                                        []string
	)
	for _, d := range decisions {
		verdict := string(d.Verdict)
		switch d.Verdict {
		case claimVerdictSingle:
			single++
			verdict = d.Owner + " (single)"
		case claimVerdictMachine:
			machine++
			verdict = d.Owner + " (machine)"
		case claimVerdictAmbiguous:
			ambiguous++
			verdict = "ambiguous"
			stuck = append(stuck, d.Node.RCUserID)
		case claimVerdictNoEdge:
			noEdge++
			verdict = "no-edge"
			stuck = append(stuck, d.Node.RCUserID)
		case claimVerdictHidden:
			hidden++
			verdict = "hidden"
		case claimVerdictAlready:
			already++
			verdict = "skip (already " + d.Node.Claimed + ")"
		}
		if d.Verdict != claimVerdictHidden && d.Verdict != claimVerdictAlready {
			pending++
		}
		fmt.Printf("%-26s | %-34s | %9d | %s\n", d.Node.MachineID, d.Node.RCUserID, len(d.Claimants), verdict)
	}

	if commit {
		for _, d := range decisions {
			if d.Owner == "" {
				continue
			}
			tag, err := pool.Exec(ctx, claimFillSQL, d.Owner, d.Node.RCUserID)
			if err != nil {
				writeFailed++
				fmt.Printf("%-26s | %-34s | write FAILED: %v\n", d.Node.MachineID, d.Node.RCUserID, err)
				continue
			}
			if tag.RowsAffected() == 0 {
				// 判定之后被真实绑定写路径抢先填了：不覆盖，如实报告。
				lostRace++
				continue
			}
			written++
		}
	}

	if lookupFailed > 0 {
		// 反查失败的账号等于「缺席的投票人」：它可能才是某个节点的唯一好友，
		// 缺了它就会把歧义判成唯一解 —— 归属写错比平铺更糟，所以拒绝写库。
		if commit {
			return fmt.Errorf("%d 个账号的好友边反查失败，拒绝写库（缺席的投票人可能把歧义判成唯一解）；修复后重跑，或先缩小范围", lookupFailed)
		}
		fmt.Printf("WARNING: %d 个账号的好友边反查失败；干跑结果仅供参考，-commit 会拒绝写库\n", lookupFailed)
	}

	fmt.Println()
	fmt.Printf("claims summary: 待填=%d 单节点判定=%d 机器交集判定=%d 歧义跳过=%d 无边跳过=%d 实际写入=%d\n",
		pending, single, machine, ambiguous, noEdge, written)
	fmt.Printf("claims detail: 隐藏跳过=%d 已有归属=%d 好友边反查失败=%d 写入失败=%d 已被抢先填写=%d\n",
		hidden, already, lookupFailed, writeFailed, lostRace)
	if !commit && pending > 0 {
		fmt.Printf("干跑：-commit 时将写 %d 行（单节点 %d + 机器交集 %d）\n", single+machine, single, machine)
	}
	if len(stuck) > 0 {
		// 故意保留：这几个智能体修完之后仍然会平铺，把清单交给产品决定是否手工指定。
		fmt.Printf("仍会平铺（故意不猜归属）：%s\n", strings.Join(stuck, ","))
	}
	if writeFailed > 0 {
		return fmt.Errorf("%d 行归属写入失败", writeFailed)
	}
	return nil
}

// collectClaimants 反查每个候选账号的融云好友列表，构造「谁把这个节点加为好友」。
// 单个账号失败只跳过并计数（一个坏 token 不该让整张表失去归属），失败数由调用方
// 决定能不能写库。
func collectClaimants(ctx context.Context, lister claimsFriendLister, candidates []string) (map[string]map[string]bool, int) {
	claimants := make(map[string]map[string]bool, len(candidates))
	if len(candidates) == 0 || lister == nil {
		return claimants, 0
	}
	ctx, cancel := context.WithTimeout(ctx, claimsLookupTimeout)
	defer cancel()

	type answer struct {
		friends []string
		err     error
	}
	answers := make([]answer, len(candidates))
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, claimsLookupConcurrency)
	for i, userID := range candidates {
		i, userID := i, userID
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				answers[i] = answer{err: gctx.Err()}
				return nil
			}
			defer func() { <-sem }()
			friends, err := lister.FriendIDs(gctx, userID)
			answers[i] = answer{friends: friends, err: err}
			return nil
		})
	}
	// 单个账号的错误已经落在 answers 里，这里不需要从 errgroup 取错误。
	_ = g.Wait()

	failed := 0
	for i, userID := range candidates {
		if answers[i].err != nil {
			failed++
			fmt.Printf("lookup FAILED user=%s error=%v\n", userID, answers[i].err)
			continue
		}
		for _, friend := range answers[i].friends {
			friend = strings.TrimSpace(friend)
			if friend == "" {
				continue
			}
			if claimants[friend] == nil {
				claimants[friend] = make(map[string]bool, 2)
			}
			claimants[friend][userID] = true
		}
	}
	return claimants, failed
}

// listClaimNodes 返回待填归属的节点，跳过隐藏节点由判定层负责（保证报表能说明
// 「为什么跳过」）。只取空值行，已填行连读都不读，天然幂等。
func listClaimNodes(ctx context.Context, pool *pgxpool.Pool) ([]nodeRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT rongcloud_user_id,
		       COALESCE(machine_id, ''),
		       COALESCE(ai_type, ''),
		       COALESCE(claimed_by_user_id, '')
		FROM rongcloud_node
		WHERE rongcloud_user_id <> ''
		  AND (claimed_by_user_id IS NULL OR btrim(claimed_by_user_id) = '')
		ORDER BY machine_id, rongcloud_user_id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes without a claim: %w", err)
	}
	defer rows.Close()
	var out []nodeRow
	for rows.Next() {
		var r nodeRow
		if err := rows.Scan(&r.RCUserID, &r.MachineID, &r.AiType, &r.Claimed); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveWorkspaces returns the workspace UUIDs to scan: the explicit flag when
// given, otherwise every workspace that currently has a rongcloud node or a
// rongcloud user (a human mirror can exist without any node, and -sync-portraits
// must reach it).
func resolveWorkspaces(ctx context.Context, pool *pgxpool.Pool, explicit string) ([]pgtype.UUID, error) {
	if strings.TrimSpace(explicit) != "" {
		var wsID pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT $1::uuid", strings.TrimSpace(explicit)).Scan(&wsID); err != nil {
			return nil, fmt.Errorf("parse -workspace %q: %w", explicit, err)
		}
		return []pgtype.UUID{wsID}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT workspace_id FROM rongcloud_node
		 UNION
		 SELECT workspace_id FROM rongcloud_user
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()
	var out []pgtype.UUID
	for rows.Next() {
		var wsID pgtype.UUID
		if err := rows.Scan(&wsID); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		out = append(out, wsID)
	}
	return out, rows.Err()
}

// resolveUsers returns the claw user ids to repair: the comma-separated flag
// when given, otherwise every active account.
func resolveUsers(ctx context.Context, pool *pgxpool.Pool, userFlag string, all bool) ([]string, error) {
	if strings.TrimSpace(userFlag) != "" {
		var out []string
		for _, id := range strings.Split(userFlag, ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	if !all {
		return nil, nil
	}
	rows, err := pool.Query(ctx, "SELECT user_id FROM claw_im_users WHERE status = 'active' ORDER BY user_id")
	if err != nil {
		return nil, fmt.Errorf("list claw users: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan claw user: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// portraitSummary counts -sync-portraits outcomes.
type portraitSummary struct {
	planned    int
	updated    int
	registered int
	missing    int
	skipped    int
	failed     int
}

func (s *portraitSummary) add(status string) {
	switch status {
	case "planned":
		s.planned++
	case "updated":
		s.updated++
	case "registered":
		s.registered++
	case "missing":
		// 融云侧没有这个账号：既不是成功也不是我们的失败，单独计数。
		// 不能并入 failed —— 库里历史残行会让整轮退出码变 1，掩盖真正的失败。
		s.missing++
	case "skipped":
		s.skipped++
	case "failed":
		s.failed++
	}
}

func (s *portraitSummary) merge(other portraitSummary) {
	s.planned += other.planned
	s.updated += other.updated
	s.skipped += other.skipped
	s.failed += other.failed
}

func printPortrait(scope, status, rcUser, from, to, errText string) {
	line := fmt.Sprintf("%-13s %-8s rc=%-46s from=%q to=%q", scope, status, rcUser, from, to)
	if errText != "" {
		line += " error=" + errText
	}
	fmt.Println(line)
}

// syncClawUserPortraits unifies the default portrait of real accounts
// (claw_im_users). RongCloud only renders the portraitUri it stores, and
// getToken never updates an existing user, so the RongCloud side must be
// pushed with /user/refresh.json before the local row is rewritten — pushing
// first keeps a failed record eligible for the next run instead of leaving a
// database value that looks "already done".
//
// Only empty values and the exact legacy default URLs are matched
// (rongcloud.NeedsDefaultPortrait); a portrait the user picked is skipped.
// users limits the scan when -user is given; otherwise every active account is
// considered.
func syncClawUserPortraits(ctx context.Context, pool *pgxpool.Pool, svc *rongcloud.NodeService, users []string, dryRun bool) (portraitSummary, error) {
	var summary portraitSummary
	query := `SELECT user_id, COALESCE(portrait_uri, '') FROM claw_im_users WHERE status = 'active'`
	args := []interface{}{}
	if len(users) > 0 {
		query += ` AND user_id = ANY($1)`
		args = append(args, users)
	}
	query += ` ORDER BY user_id`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return summary, fmt.Errorf("list claw users: %w", err)
	}
	defer rows.Close()

	target := rongcloud.DefaultUserPortrait()
	for rows.Next() {
		var userID, current string
		if err := rows.Scan(&userID, &current); err != nil {
			return summary, fmt.Errorf("scan claw user: %w", err)
		}
		if !rongcloud.NeedsDefaultPortrait(current) {
			summary.skipped++
			printPortrait("claw_im_users", "skipped", userID, current, target, "")
			continue
		}
		if dryRun {
			summary.planned++
			printPortrait("claw_im_users", "planned", userID, current, target, "")
			continue
		}
		// name 传空串：融云侧昵称保持原值，只改头像。
		if err := svc.PushUserPortrait(ctx, userID, "", target); err != nil {
			summary.failed++
			printPortrait("claw_im_users", "failed", userID, current, target, err.Error())
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE claw_im_users SET portrait_uri = $2, updated_at = now() WHERE user_id = $1`,
			userID, target); err != nil {
			summary.failed++
			printPortrait("claw_im_users", "failed", userID, current, target, err.Error())
			continue
		}
		summary.updated++
		printPortrait("claw_im_users", "updated", userID, current, target, "")
	}
	return summary, rows.Err()
}

// portraitNeedsFixWhere 是存量判定在 SQL 侧的预筛：空值、两个历史常量全等、
// 或 URL path 精确以旧静态路由结尾（right() 做的是精确后缀，不是 LIKE）。
// 逐行仍会用 rongcloud.NeedsDefaultPortrait 复核，所以即使预筛放宽也不会误写
// 用户自定义头像。参数固定为 ($1 legacy URIs, $2 path 长度, $3 path)。
const portraitNeedsFixWhere = `(portrait_uri IS NULL
	OR btrim(portrait_uri) = ''
	OR portrait_uri = ANY($1)
	OR right(btrim(portrait_uri), $2::int) = $3)`

// portraitFixCandidates 返回预筛参数（历史 URI 列表 / path 长度 / path）。
func portraitFixCandidates() []interface{} {
	legacy := rongcloud.LegacyDefaultPortraitURIs()
	path := rongcloud.LegacyDefaultPortraitPath
	return []interface{}{legacy, len(path), path}
}

// groupPortraitNeedsFixWhere 是群 / 聊天室两路的预筛：比 portraitNeedsFixWhere
// 多认一档 —— 「统一默认图 default_user.png 与三张群类图」。换图之前所有群都落在
// default_user.png 上，不做这档等值匹配，CLI 永远选不到最主要的存量。
//
// 依然只是预筛：逐行由 rongcloud.NeedsGroupPortraitFix 复核（要求「不等于目标」），
// 所以用户自定义头像不会被写，已经是目标图的行也不会重复推融云。
// 参数固定为 ($1 legacy URIs, $2 path 长度, $3 path, $4 可改写的默认图全集)。
const groupPortraitNeedsFixWhere = `(portrait_uri IS NULL
	OR btrim(portrait_uri) = ''
	OR portrait_uri = ANY($1)
	OR right(btrim(portrait_uri), $2::int) = $3
	OR btrim(portrait_uri) = ANY($4))`

// groupPortraitFixCandidates 返回群 / 聊天室预筛参数。
func groupPortraitFixCandidates() []interface{} {
	legacy := rongcloud.LegacyDefaultPortraitURIs()
	path := rongcloud.LegacyDefaultPortraitPath
	return []interface{}{legacy, len(path), path, rongcloud.GroupDefaultPortraitURIs()}
}

// syncGroupPortraits 补齐群头像（rongcloud_group）：融云 /group/create.json 不
// 收 portraitUri，群头像只能事后用 /group/refresh.json 补写 —— 所以这一路必须
// 真的调融云。顺序是「先推融云成功，再回写本地列」，失败只计数（best-effort），
// 库内保留旧值以便下次重试。
//
// 目标是**群聊专用**默认图（两人剪影），不是真人那张统一默认图：移动端会话列表
// 读的就是融云群资料的 portraitUri，全落在同一张灰人形上的话，一排群长得一样。
//
// 先把候选行全部读出来再逐条处理：refreshGroupInfo 是外网调用，不能占着连接池
// 的游标不放。
func syncGroupPortraits(ctx context.Context, pool *pgxpool.Pool, svc *rongcloud.NodeService, dryRun bool) (portraitSummary, error) {
	var summary portraitSummary
	rows, err := pool.Query(ctx, `
		SELECT id::text, group_id, COALESCE(portrait_uri, '')
		FROM rongcloud_group
		WHERE `+groupPortraitNeedsFixWhere+`
		ORDER BY group_id`, groupPortraitFixCandidates()...)
	if err != nil {
		return summary, fmt.Errorf("list groups: %w", err)
	}
	type groupRow struct {
		id       string
		groupID  string
		portrait string
	}
	var pending []groupRow
	for rows.Next() {
		var r groupRow
		if err := rows.Scan(&r.id, &r.groupID, &r.portrait); err != nil {
			rows.Close()
			return summary, fmt.Errorf("scan group: %w", err)
		}
		if !rongcloud.NeedsGroupPortraitFix(r.portrait, rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindGroup)) {
			summary.skipped++
			continue
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}

	target := rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindGroup)
	for _, r := range pending {
		if dryRun {
			summary.planned++
			printPortrait("rongcloud_group", "planned", r.groupID, r.portrait, target, "")
			continue
		}
		if err := svc.RefreshGroupPortrait(ctx, r.groupID, target); err != nil {
			summary.failed++
			printPortrait("rongcloud_group", "failed", r.groupID, r.portrait, target, err.Error())
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE rongcloud_group SET portrait_uri = $2, updated_at = now() WHERE id = $1::uuid`,
			r.id, target); err != nil {
			summary.failed++
			printPortrait("rongcloud_group", "failed", r.groupID, r.portrait, target, err.Error())
			continue
		}
		summary.updated++
		printPortrait("rongcloud_group", "updated", r.groupID, r.portrait, target, "")
	}
	return summary, nil
}

// syncChatroomPortraits 按房间类型回写聊天室头像（rongcloud_chatroom）：
// conversation_kind = discussion → 圆桌三席，其余（chatroom / 空）→ 两个气泡。
//
// 只写本地库：融云聊天室 API 没有头像字段，这一列是各端读头像的唯一来源
// （web 会话列表、小程序聊天室列表都读它）。
//
// onlyRoomIDs 非空时只处理这些房间（测试必须只碰自己种下的行，不能把共享库里的
// 存量房间顺手刷一遍；线上调用不传，语义仍是全量）。
func syncChatroomPortraits(ctx context.Context, pool *pgxpool.Pool, dryRun bool, onlyRoomIDs ...string) (portraitSummary, error) {
	var summary portraitSummary
	query := `
		SELECT id::text, rongcloud_chatroom_id, COALESCE(conversation_kind, ''), COALESCE(portrait_uri, '')
		FROM rongcloud_chatroom
		WHERE ` + groupPortraitNeedsFixWhere
	args := groupPortraitFixCandidates()
	if len(onlyRoomIDs) > 0 {
		query += ` AND rongcloud_chatroom_id = ANY($5::text[])`
		args = append(args, onlyRoomIDs)
	}
	query += ` ORDER BY rongcloud_chatroom_id`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return summary, fmt.Errorf("list chatrooms: %w", err)
	}
	type chatroomRow struct {
		id       string
		rcID     string
		kind     string
		portrait string
		target   string
	}
	var pending []chatroomRow
	for rows.Next() {
		var r chatroomRow
		if err := rows.Scan(&r.id, &r.rcID, &r.kind, &r.portrait); err != nil {
			rows.Close()
			return summary, fmt.Errorf("scan chatroom: %w", err)
		}
		r.target = rongcloud.GroupPortraitURI(rongcloud.ChatroomPortraitKind(r.kind))
		if !rongcloud.NeedsGroupPortraitFix(r.portrait, r.target) {
			summary.skipped++
			continue
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}

	for _, r := range pending {
		if dryRun {
			summary.planned++
			printPortrait("rongcloud_chatroom", "planned", r.rcID, r.portrait, r.target, "")
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE rongcloud_chatroom SET portrait_uri = $2, updated_at = now() WHERE id = $1::uuid`,
			r.id, r.target); err != nil {
			summary.failed++
			printPortrait("rongcloud_chatroom", "failed", r.rcID, r.portrait, r.target, err.Error())
			continue
		}
		summary.updated++
		printPortrait("rongcloud_chatroom", "updated", r.rcID, r.portrait, r.target, "")
	}
	return summary, nil
}

// syncLocalPortraitColumn 回写只存在于本地读模型的头像列（rongcloud_chatroom_member）：
// 融云聊天室 API 没有头像字段，所以**不推融云**，只按同一套精确判定逐行改本地库。
// display 是日志里可读的业务键（成员表用 member_id），table/display 只接受本文件内
// 的常量。
func syncLocalPortraitColumn(ctx context.Context, pool *pgxpool.Pool, table, display string, dryRun bool) (portraitSummary, error) {
	var summary portraitSummary
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT id::text, COALESCE(%s, id::text), COALESCE(portrait_uri, '')
		FROM %s
		WHERE %s
		ORDER BY id`, display, table, portraitNeedsFixWhere), portraitFixCandidates()...)
	if err != nil {
		return summary, fmt.Errorf("list %s: %w", table, err)
	}
	type localRow struct {
		id       string
		key      string
		portrait string
	}
	var pending []localRow
	for rows.Next() {
		var r localRow
		if err := rows.Scan(&r.id, &r.key, &r.portrait); err != nil {
			rows.Close()
			return summary, fmt.Errorf("scan %s: %w", table, err)
		}
		if !rongcloud.NeedsDefaultPortrait(r.portrait) {
			summary.skipped++
			continue
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}

	target := rongcloud.DefaultUserPortrait()
	for _, r := range pending {
		if dryRun {
			summary.planned++
			printPortrait(table, "planned", r.key, r.portrait, target, "")
			continue
		}
		if _, err := pool.Exec(ctx,
			fmt.Sprintf(`UPDATE %s SET portrait_uri = $2, updated_at = now() WHERE id = $1::uuid`, table),
			r.id, target); err != nil {
			summary.failed++
			printPortrait(table, "failed", r.key, r.portrait, target, err.Error())
			continue
		}
		summary.updated++
		printPortrait(table, "updated", r.key, r.portrait, target, "")
	}
	return summary, nil
}

// loadEnvFile sets KEY=VALUE pairs from path into the process environment
// without overwriting variables that are already set. It understands '#'
// comments, a leading "export ", and single/double quoted values.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
