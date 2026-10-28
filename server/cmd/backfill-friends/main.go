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
// Typical use (from the server/ directory):
//
//	go run ./cmd/backfill-friends -env-file ../.env -commit -all-users
//	go run ./cmd/backfill-friends -env-file ../.env -commit -user 100123,100456
//	go run ./cmd/backfill-friends -env-file ../.env -commit -sync-names
//	go run ./cmd/backfill-friends -env-file ../.env -sync-portraits
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
	dsnFlag := flag.String("dsn", "", "postgres DSN (default: $DATABASE_URL, then $MULTICA_DATABASE_URL)")
	envFile := flag.String("env-file", "", "load KEY=VALUE environment variables from this file before reading configuration")
	flag.Parse()

	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	}

	if *userFlag == "" && !*allUsers && !*syncNames && !*syncPortraits && !*syncAgentPortraits {
		return fmt.Errorf("specify -user <id[,id...]>, -all-users, -sync-names, or -sync-portraits to choose what to repair")
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
