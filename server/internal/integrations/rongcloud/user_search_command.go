package rongcloud

// user_search_command.go implements service=user, action=search —— web「添加好友」
// 的账号搜索。
//
// 客户端调用点：clawmessenger-web/src/services/contacts.ts:237-249
// （`sendSystemRequest('user','search',{keyword})`，读 `result?.list`，
// 解包在 clawmessenger-web/src/services/im.ts:469-471：成功时 resolve
// `response.data`）。uniapp 同一契约：clawmessenger-uniapp/src/utils/
// system-service-client.js:392-393 + subPackages/social/pages/search-user/index.vue:361-375
// （逐字段 `||` 兜底，所以只要给 userId/username/nickname/portraitUri 就够）。
//
// 移植来源是旧 Python 后端的 `_handle_user_search`
// （clawmessenger-server/rongcloud/command_handler.py:1936-1993）。移植时**有意**
// 偏离旧实现五处，理由都写在对应代码旁（决策记录见
// .agent-runs/2026-10-27/task-001/implementer-report.md 的 D1/D2/D3/D4 与 §8）：
//
//  1. 加 `LIMIT 20`：旧实现无上限，一次搜索可能把整张表通过一条 IM 私聊下发；
//  2. 命中字段多一列 `user_id`：web 界面上「账号」显示的是 userId
//     （AddFriendDialog.tsx:646），而注册允许自定义 username（claw_compat.go:330-359），
//     只搜旧实现那四列会出现「用户看着自己的数字 id 却搜不到自己」；
//  3. 不做头像回填融云：旧实现逐行调 /user/info.json 并写库
//     （command_handler.py:1968-1980），是 N+1 次外部调用，会把命令通道拖到客户端
//     15s 超时附近。代价是老账号搜索结果可能没有头像（前端有默认头像兜底）；
//  4. 回包不含 `email` / `phone`：旧实现把这两列一起回传
//     （command_handler.py:1981-1990）。**命中列比回包宽** —— 这两列只用于 WHERE
//     匹配、不返回给调用方。理由：本 action 不校验身份、也没有频率限制，若把命中的
//     手机号/邮箱回传，「递增关键字 + 观察结果」就能把手机号/邮箱从模糊匹配里逐位
//     枚举出来；而两端 UI 都不消费它们（web `AddFriendDialog` 只用
//     userId/username/nickname/portraitUri；uniapp `search-user/index.vue:366-375`
//     的映射也只取这 4 个）；
//  5. `nickname` 为空时不回退成 `username`：旧实现明确兜底
//     （command_handler.py:1984 `row["nickname"] or row["username"]`），这里返回原始值
//     （可能是空串）。与 claw_compat.go:360-363「昵称不用账号名兜底」的最新口径一致，
//     前端自己兜底（AddFriendDialog.tsx:643 `user.nickname || user.username`）。
//
// 一处**刻意照旧**：不校验发送者身份（serviceActor 失败会回 401，而 web 的
// searchUsers 没有错误码映射，会把英文原文直接 toast 给用户）。
//
// 数据源只有 claw_im_users 一张表：rongcloud_user 只是节点/安装账号的资料镜像，
// 真人账号不写它（system_handler.go:124-129、chatroom_commands.go:53-63）。
// 因此 AI 节点（rc_node_*）搜不到 —— 与旧实现一致，属既有边界而非本次缺陷。

import (
	"context"
	"fmt"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// UserSummary 是 user/search 的一条结果，json tag 用 camelCase。
//
// 只有四个字段：**命中列比回包宽** —— `email` / `phone` 参与 WHERE 匹配但不回传
// （理由见文件头「有意偏离」第 4 条：避免把手机号/邮箱回给任意搜索者）。旧实现回传
// 这两个字段（command_handler.py:1981-1990），本次有意收窄。web 的 `UserInfo`
// （types/api.ts:8-19）里 phone/email 是可选字段，缺省不影响类型。
//
// 不加 `id`/`avatar`：旧实现的 search 也没给，两端的 `||` 兜底已兼容
// （system-service-client.js:392、search-user/index.vue:366-375）。
type UserSummary struct {
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	Nickname    string `json:"nickname"`
	PortraitURI string `json:"portraitUri"`
}

const (
	// userSearchLimit 是本服务的搜索条数上限。旧实现没有 LIMIT，这里有意收紧：
	// 回包是一条 IM 私聊消息，用户量一大就意味着单条命令消息体失控。
	userSearchLimit = 20
)

// handleUserSearch 是 service=user/action=search 的入口（路由见 user_command.go 的
// handleUserCommand）。
func (h *systemHandler) handleUserSearch(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	keyword := strings.TrimSpace(getStringAny(cmd.Params, "keyword", "q"))
	if keyword == "" {
		// 文案沿用旧实现（command_handler.py:1941）。
		return h.replyErr(ctx, msg, cmd, 400, "keyword 不能为空")
	}
	// 与 friend / lobster / conversation 同款文案：store 未装配（无 DB）时回 503
	// 而不是 panic。h.queries == nil 不用单独判 —— store 为 nil 已经覆盖了。
	if h.userStore == nil || h.userStore.SearchUsers == nil {
		return h.replyErr(ctx, msg, cmd, 503, "user service is unavailable")
	}
	users, err := h.userStore.SearchUsers(ctx, keyword, userSearchLimit)
	if err != nil {
		// 只记日志、只回通用文案：旧实现把 str(e) 直接回给客户端
		// （command_handler.py:1992），会把内部表名/SQL 细节漏出去。
		// keyword 本身也不进日志 —— 它可能是手机号或邮箱。
		h.logger.Error("rongcloud: user search", "error", err, "keyword_len", len(keyword))
		return h.replyErr(ctx, msg, cmd, 500, "failed to search users")
	}
	if users == nil {
		users = []UserSummary{}
	}
	// 必须是 map，不能传裸切片：设备信封分支对 map 逐键摊平、对其它类型塞进
	// payload.data（system_handler.go:634-642），CLI 侧要看到 payload.list。
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"list": users})
}

// searchClawIMUsers 是 UserStore.SearchUsers 的生产实现（表：claw_im_users，
// migrations/583 + 599）。参数化 SQL 走 DBTX 是集成层的既有约定：这个 service
// 的本地表读写没有 sqlc 查询（registration.go:42-46），因此不需要 `make sqlc`。
//
// 过滤条件全部来自旧实现（command_handler.py:1948-1958）：
//   - `status = 'active'`：被 admin 停用的账号不该出现在搜索结果里；
//   - 双 `om_` 前缀排除：`om_` 是同一台机器上的运维账号（web
//     src/lib/ai-node-id.ts:48-49）。前端也过滤一层，但那是兜底，不能只靠前端；
//   - `ORDER BY created_at DESC`：与旧实现一致。
//
// 注意「命中列 ≠ 回包列」：WHERE 里 ILIKE 的是 5 列（含 email / phone），SELECT
// 只取 4 列。按下手机号/邮箱能搜到人（旧实现的产品能力），但命中的手机号/邮箱
// 不回给调用方 —— 详见文件头「有意偏离」第 4 条。改 SELECT 时必须同步改 rows.Scan。
func searchClawIMUsers(ctx context.Context, dbtx db.DBTX, keyword string, limit int) ([]UserSummary, error) {
	if limit <= 0 {
		limit = userSearchLimit
	}
	// 空关键字在 SQL 这一层再兜一道：clawIMLikePattern("") 得到的是 `%%`，在后端
	// 语义上就是「匹配全表」，等于把前 limit 条账号无条件吐出去。
	//
	// 当前唯一的拦截是 handleUserSearch 里的 TrimSpace 空判（那里回 400，是对外
	// 契约，不要动），但这道防线长在调用方身上 —— 将来若有人从 HTTP 入口或其它
	// service 直接复用 SearchUsers，就会绕过去。所以这里不构造 pattern、也不发起
	// Query，直接回空结果。
	//
	// 这条分支不改变 user/search 的对外行为：handler 先回 400，走不到这里。
	if strings.TrimSpace(keyword) == "" {
		return make([]UserSummary, 0, limit), nil
	}
	rows, err := dbtx.Query(ctx, `
		SELECT user_id, username, COALESCE(nickname, ''), COALESCE(portrait_uri, '')
		FROM claw_im_users
		WHERE status = 'active'
		  AND LEFT(username, 3) <> 'om_'
		  AND LEFT(user_id, 3) <> 'om_'
		  AND (username ILIKE $1
		       OR COALESCE(nickname, '') ILIKE $1
		       OR COALESCE(email, '') ILIKE $1
		       OR COALESCE(phone, '') ILIKE $1
		       OR user_id ILIKE $1)
		ORDER BY created_at DESC
		LIMIT $2`, clawIMLikePattern(keyword), limit)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: search claw users: %w", err)
	}
	defer rows.Close()

	// 预先 make 成非 nil：零命中时 replyOK 序列化出来是 []，不是 null
	// （前端 `result?.list ?? []` 两种都能吃，但 [] 更省心）。
	users := make([]UserSummary, 0, limit)
	for rows.Next() {
		var user UserSummary
		// 4 个目标与 SELECT 的 4 列严格一一对应：user_id → UserID、
		// username → Username、nickname → Nickname、portrait_uri → PortraitURI。
		// 加/删列时这两处必须一起改（错位是最典型的 silent bug）。
		if err := rows.Scan(&user.UserID, &user.Username, &user.Nickname,
			&user.PortraitURI); err != nil {
			return nil, fmt.Errorf("rongcloud: scan claw user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rongcloud: iterate claw users: %w", err)
	}
	return users, nil
}

// clawIMLikePattern 把关键字包成 ILIKE 的 `%kw%`，并转义 LIKE 元字符。
//
// 与 internal/handler/claw_admin_compat.go:178-183 的 clawAdminLikePattern 同逻辑：
// 那个函数未导出、且集成层不该反向依赖 handler 包，所以这里保留一份副本。
// 顺序不能换：先转义反斜杠，再转义 % 和 _ —— 反过来会把刚加上的转义符再转一次。
// PostgreSQL 的 LIKE/ILIKE 默认转义字符就是反斜杠，不需要额外写 ESCAPE 子句。
//
// 绝不要退化成 `"%" + keyword + "%"` 裸拼接：那样用户输一个 `%` 就能把全表捞出来。
func clawIMLikePattern(keyword string) string {
	escaped := strings.ReplaceAll(keyword, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "%", "\\%")
	escaped = strings.ReplaceAll(escaped, "_", "\\_")
	return "%" + escaped + "%"
}
