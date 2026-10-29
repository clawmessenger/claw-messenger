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
//  2. 命中字段多一列 `user_id`：老账号可能没有 username（注册允许自定义
//     username，见 claw_compat.go:330-359），只搜旧实现那四列会出现「用户看着
//     自己的数字 id 却搜不到自己」；
//  3. 不做 N+1 式的头像回填融云：旧实现**逐行**调 /user/info.json 并写库
//     （command_handler.py:1968-1980），20 条结果就是 20 次外部调用，会把命令
//     通道拖到客户端 15s 超时附近。移植时整段砍掉了。
//     2026-10-29 修订：搜索改成按虾说号精确匹配后，一次最多命中 1 个真人 + 1 个
//     节点，调用次数降到 ≤2，原来那条「N+1」的理由已经不成立，于是**只针对缺名字
//     或缺头像的行**补一次（enrichUserSummariesFromRongCloud，带 2s 预算）。
//     仍然不写库 —— 理由见该函数注释；
//  4. 回包不含 `email` / `phone`：旧实现把这两列一起回传
//     （command_handler.py:1981-1990）。本 action 不校验身份、也没有频率限制，把
//     手机号/邮箱回给任意搜索者本身就是泄漏面；而两端 UI 都不消费它们（web
//     `AddFriendDialog` 只用 userId/username/nickname/portraitUri；uniapp
//     `search-user/index.vue:366-375` 的映射也只取这 4 个）；
//  5. `nickname` 为空时不回退成 `username`：旧实现明确兜底
//     （command_handler.py:1984 `row["nickname"] or row["username"]`），这里返回原始值
//     （可能是空串）。与 claw_compat.go:360-363「昵称不用账号名兜底」的最新口径一致，
//     前端自己兜底（AddFriendDialog.tsx:643 `user.nickname || user.username`）。
//
// # 2026-10-28 收口：从「五列模糊」改成「虾说号精确」
//
// 产品口径变了：加好友**只允许按虾说号找人**，不接受昵称/手机号/邮箱。此前 WHERE
// 命中 username / nickname / email / phone / user_id 五列且是 `%kw%` 模糊匹配，
// 于是「递增关键字 + 观察结果」就能把手机号、邮箱、真实姓名逐位枚举出来 ——
// 客户端那层 `pickExactShuoshuoId` 只能挡住界面，直接打 IM 命令通道照样能捞人。
//
// 现在的判定（`username ILIKE $1 OR user_id = $1`）：
//   - `$1` 是**原样关键字**，不再包 `%...%`。ILIKE 不带通配符就是「忽略大小写的
//     等值」，所以 `%`/`_` 这类 LIKE 元字符天然失效，不需要转义（也不能再包通配符，
//     否则 `alice%` 又能把前缀匹配捡回来）；
//   - nickname / email / phone 彻底退出 WHERE（连「命中但不回传」都不再成立）；
//   - 保留 `user_id` 精确等值，只为那批没有 username 的老账号还能被找到；userId 是
//     不可猜的串，不构成枚举入口；
//   - `claw_im_users.username` 有 UNIQUE 约束（migrations/583），所以这个判定最多
//     命中一条真人账号，`LIMIT 20` 保留只为兜住「username 与 user_id 同时命中」。
//
// 客户端（web `AddFriendDialog.pickExactShuoshuoId`、小程序 `search-user`）仍保留
// 一层精确过滤：发布顺序上总是前端先上、后端后上，旧后端还在跑的那段时间里，
// 那层过滤就是唯一的口径执行者。
//
// 一处**刻意照旧**：不校验发送者身份（serviceActor 失败会回 401，而 web 的
// searchUsers 没有错误码映射，会把英文原文直接 toast 给用户）。
//
// 数据源是两张表，各管一类账号（2026-10-29 起）：
//   - claw_im_users：真人账号（C 端注册写入）；
//   - rongcloud_node（LEFT JOIN rongcloud_user 取名字/头像）：AI 节点账号。
//
// 为什么要加第二张表：C 端「智能体资料卡」把节点自己的融云账号（形如
// `codex_3300776275`、`rc_node_<machine>_<agent>`）当「虾说号」展示并给了复制按钮，
// 用户复制去搜索却必然落空 —— 之前只有 claw_im_users 一个数据源。产品口径是
// **所有设备账号都可被搜到并申请添加**（是否通过由节点拥有者审批，见
// friend/sendRequest），所以搜索必须覆盖节点账号。
//
// 节点的「虾说号」就是它的融云账号（= `rongcloud_node.rongcloud_user_id`），
// 与资料卡展示的是同一个值；两端客户端（web AddFriendDialog、uniapp
// search-user）都已经能用 `isAiNodeId()` 认出它，并自动改走「向拥有者申请」的
// 通道，因此这里只需要把行搜出来，不需要给回包加新字段。
//
// 隐藏节点仍然搜不到：机器主节点与运维节点是内部管道（红线 #6），判据与好友列表的
// `isHiddenNode`（node_service.go:254）一致 —— 运维节点看 `ai_type = 'ops'`；机器
// 主节点不能靠 `ai_type` 判（真实库里它是 `opencode` 这类值），只能看 rc id 形状：
// `rc_node_<machineKey>` 没有 agent 后缀。具体写法见 SQL 里的注释。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// UserSummary 是 user/search 的一条结果，json tag 用 camelCase。
//
// 只有四个字段：`email` / `phone` 既不参与 WHERE 也不参与回包（理由见文件头
// 「有意偏离」第 4 条与 2026-10-28 的收口说明）。旧实现回传这两个字段
// （command_handler.py:1981-1990），本次有意收窄。web 的 `UserInfo`
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
	// 命中之后回融云补一次资料（见 enrichUserSummariesFromRongCloud）。
	// 为什么现在才敢加：旧实现在这里逐行调 /user/info.json 是 N+1（20 条结果
	// = 20 次外部调用），会把命令通道拖到客户端 15s 超时附近，所以移植时被砍掉
	// （见文件头「有意偏离」第 3 条）。改成按虾说号精确匹配之后，一次搜索最多
	// 命中 1 个真人 + 1 个节点，同一条链路只剩 ≤2 次调用，N+1 这个理由已经不成立。
	if h.client != nil {
		enrichUserSummariesFromRongCloud(ctx, h.client, h.logger, users)
	}
	// 必须是 map，不能传裸切片：设备信封分支对 map 逐键摊平、对其它类型塞进
	// payload.data（system_handler.go:634-642），CLI 侧要看到 payload.list。
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"list": users})
}

// userSearchEnrichTimeout 是回填阶段的总预算，不占用整条命令链路的超时。
//
// 融云 /user/info.json 限 100 QPS、正常几十毫秒返回，2s 足够覆盖两次调用；
// 真挂住时宁可少补一个名字，也不能让「搜一个号」卡到客户端放弃等待。
const userSearchEnrichTimeout = 2 * time.Second

// rongcloudUserInfoFetcher 是回填阶段唯一需要的外部能力，抽成接口只为让测试
// 不用起 httptest。生产实现是 *rongcloudAPIClient.getUserInfo。
type rongcloudUserInfoFetcher interface {
	getUserInfo(ctx context.Context, userID string) (userInfoResult, error)
}

// enrichUserSummariesFromRongCloud 用融云注册资料补齐搜索结果里缺的名字/头像。
//
// 为什么需要（2026-10-29 加）：搜索的数据源是本地两张表 —— 真人读
// claw_im_users，节点读 rongcloud_node LEFT JOIN rongcloud_user。镜像是**投影**，
// 节点换过名字、或者 rongcloud_user 里干脆没有这一行时，回包就只有一串
// `rc_node_...`，前端只好显示 id（web AddFriendDialog 有默认头像兜底，但名字
// 兜不住）。而虾说号本身就是融云 userId，拿它去问一次 /user/info.json 就能拿到
// 注册时的真名和头像 —— 这是本地表补不了的。
//
// 只补**空字段**，不覆盖已有值：真人的 nickname/portrait_uri 以本地为准（我们是
// 往融云推数据的一方，反过来拿融云旧值覆盖，会让用户刚改的昵称被回滚）。
// 也正因为只补空，绝大多数搜索根本不会发这次请求。
//
// 失败一律不阻塞：查不到（账号已注销 / 网络 / 融云 5xx）就保持原样返回。把
// 「补资料」的失败升级成搜索失败没有道理 —— 用户至少还能看到那串 id 并申请加好友。
//
// 不做「查到就写回本地表」：UserSummary 不带来源标记，分不清这一行来自
// claw_im_users 还是 rongcloud_node，写回要按来源分两条 UPDATE；而这条链路是
// 读路径，写库会引入新的锁竞争面。收益（省下一次查询）远小于代价。
func enrichUserSummariesFromRongCloud(ctx context.Context, fetcher rongcloudUserInfoFetcher, logger *slog.Logger, users []UserSummary) {
	if fetcher == nil || len(users) == 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(ctx, userSearchEnrichTimeout)
	defer cancel()
	for i := range users {
		user := &users[i]
		needName := strings.TrimSpace(user.Nickname) == ""
		needPortrait := strings.TrimSpace(user.PortraitURI) == ""
		if !needName && !needPortrait {
			continue
		}
		userID := strings.TrimSpace(user.UserID)
		if userID == "" {
			continue
		}
		info, err := fetcher.getUserInfo(ctx, userID)
		if err != nil {
			// Debug 而不是 Warn：融云对已注销/不存在的号本来就会报错，一次
			// 搜索里出现它属于预期，Warn 会把日志刷满。
			logger.Debug("rongcloud: user search enrich skipped",
				"error", err, "user_id", userID)
			continue
		}
		if needName {
			user.Nickname = info.Name
		}
		if needPortrait {
			user.PortraitURI = info.PortraitURI
		}
	}
}

// searchClawIMUsers 是 UserStore.SearchUsers 的生产实现（表：claw_im_users +
// rongcloud_node，migrations/583 + 599 + 564）。参数化 SQL 走 DBTX 是集成层的既有
// 约定：这个 service 的本地表读写没有 sqlc 查询（registration.go:42-46），因此
// 不需要 `make sqlc`。
//
// 过滤条件全部来自旧实现（command_handler.py:1948-1958）：
//   - `status = 'active'`：被 admin 停用的账号不该出现在搜索结果里；
//   - 双 `om_` 前缀排除：`om_` 是同一台机器上的运维账号（web
//     src/lib/ai-node-id.ts:48-49）。前端也过滤一层，但那是兜底，不能只靠前端；
//   - `ORDER BY created_at DESC`：与旧实现一致（节点与真人混排，最近接入的在前）。
//
// 命中判定是**精确**的（2026-10-28 收口，详见文件头）：`username ILIKE $1` 是
// 忽略大小写的等值（$1 不带任何通配符），外加 `user_id = $1` 兜住没有 username
// 的老账号。nickname / email / phone 已完全退出 WHERE —— 也就是说「按手机号搜得到
// 人」这个旧能力被有意取消了，不要在没有产品决定的情况下加回来。节点分支同理：
// 只按 `rongcloud_user_id ILIKE $1` 命中，节点名/机器名都**不**参与。
//
// 一条查询取两张表（UNION ALL）而不是发两次 Query：回包是一条 IM 私聊消息，
// 搜索又落在客户端 15s 超时的链路上，往返次数不能因为加了节点而翻倍。
// 外层再套一层是为了让 `ORDER BY` / `LIMIT` 对两类账号统一生效，同时保住
// 「SELECT 四列、列序 = Scan 顺序」这条契约（内层多带一个 created_at 只用于排序）。
//
// 改 SELECT 时必须同步改 rows.Scan。
func searchClawIMUsers(ctx context.Context, dbtx db.DBTX, keyword string, limit int) ([]UserSummary, error) {
	if limit <= 0 {
		limit = userSearchLimit
	}
	// 空关键字在 SQL 这一层再兜一道：精确匹配下空串虽然只可能命中「user_id 为空」
	// 的脏数据，但这类守卫不能寄希望于上游 —— 当前唯一的拦截是 handleUserSearch
	// 里的 TrimSpace 空判（那里回 400，是对外契约，不要动），而它长在调用方身上。
	// 将来若有人从 HTTP 入口或其它 service 直接复用 SearchUsers，就会绕过去。
	//
	// 这条分支不改变 user/search 的对外行为：handler 先回 400，走不到这里。
	if strings.TrimSpace(keyword) == "" {
		return make([]UserSummary, 0, limit), nil
	}
	// $1 直接绑原样关键字：**不要**在这里包 `%...%`。包了就把模糊匹配又放回来了
	// （`alice%` 能前缀命中），而 ILIKE 不带通配符本身就是等值匹配，`%`/`_` 因此
	// 不会退化成元字符，也就不需要转义。
	rows, err := dbtx.Query(ctx, `
		SELECT user_id, username, COALESCE(nickname, ''), COALESCE(portrait_uri, '')
		FROM (
			SELECT user_id, username, nickname, portrait_uri, created_at
			  FROM claw_im_users
			 WHERE status = 'active'
			   AND LEFT(username, 3) <> 'om_'
			   AND LEFT(user_id, 3) <> 'om_'
			   AND (username ILIKE $1 OR user_id = $1)
			UNION ALL
			-- 节点账号：虾说号 = 融云账号。两类内部节点必须排除，判据与好友列表的
			-- isHiddenNode（node_service.go:254）同源：
			--   1) 运维节点 —— ai_type = 'ops'；
			--   2) 机器主节点 —— rc id 形如 rc_node_<machineKey>，没有 agent 后缀。
			--      注意不能只看 ai_type 判它：真实库里机器主节点的 ai_type 是
			--      'opencode' 这类值（不是空串），只看 ai_type 会把主节点搜出来。
			--      机器 key 不含下划线（web ai-node-id.ts:152 同前提），所以「去掉
			--      rc_node_ 前缀后是否还有下划线」就是「有没有 agent 后缀」。
			SELECT n.rongcloud_user_id, n.rongcloud_user_id, ru.name, ru.portrait_uri, n.created_at
			  FROM rongcloud_node n
			  LEFT JOIN rongcloud_user ru ON ru.rongcloud_user_id = n.rongcloud_user_id
			 WHERE n.rongcloud_user_id ILIKE $1
			   AND LEFT(n.rongcloud_user_id, 3) <> 'om_'
			   AND COALESCE(n.ai_type, '') NOT IN ('', 'ops')
			   AND (LEFT(n.rongcloud_user_id, 8) <> 'rc_node_'
			        OR strpos(substr(n.rongcloud_user_id, 9), '_') > 0)
		) hit
		ORDER BY created_at DESC
		LIMIT $2`, keyword, limit)
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
