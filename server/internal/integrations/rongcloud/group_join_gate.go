package rongcloud

import (
	"context"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 群聊「加入侧」权限闸门（迁移 607 的 allow_group）。
//
// 在此之前 allow_group 只在**派发侧**生效：agent 被拉进群以后不吭声，但照样
// 被拉进去。用户看到的现象是「这群里明明有这个智能体，它却一句话不说」——
// 既没解释也没入口，比直接拦住更糟。会议纪要第 4 条要的是「禁止进入他人
// 群聊」，主语是「进入」，所以闸门必须落在加入这一侧。
//
// 口径与派发侧**完全一致**（见 chatroom_relay.go 的 agentPermissionSet.allows）：
// 开关只约束「别的账号」。主人自己把自己的 agent 拉进群，不受自己关掉的开关
// 限制；未认领的节点没有主人、也就没有可点的开关，一律放行。这两条如果两边
// 不一致，就会出现「加得进去但永远不说话」或者「说不出来却也加不进去」的
// 自相矛盾组合，排查成本极高。
//
// 存量成员不做清理：已经在群里的 agent 留在原地（踢人是有副作用的破坏性动作，
// 而且派发侧已经会拦它说话）。闸门只管「这一次要把谁加进来」。

// agentJoinPermission 是判定「这个成员能不能进群」所需的全部信息。
//
// 只取这两个字段是有意的：加入侧不该知道 allow_fs / behavior_prompt 这些与
// 「能不能进群」无关的东西，知道得越多越容易在别处顺手复用出一份走样的判定。
type agentJoinPermission struct {
	// claimedBy 是认领者（主人）的融云 id，空串表示未认领。
	//
	// claimed_by_user_id 存的是 claw_im_users.user_id，而它在融云侧就是 user id
	// （见迁移 600 的列注释），所以可以直接和 FromUserID 做字符串比较。
	// **不要**换成 owner_user_id：那是工作区管理员，语义完全不同。
	claimedBy string
	// allowGroup 是主人给的「允许群聊发言」开关。
	allowGroup bool
}

// agentJoinRejection 是回给前端的拒绝明细（逐人给，不只给一个总数）。
//
// 必须逐人给：前端一次可能勾了 5 个好友，其中 1 个是被别人认领且关了开关的
// agent。只回一句「部分成员未加入」的话，用户点开成员列表发现少了一个，既
// 不知道少的是谁，也不知道为什么 —— 这正是这类功能最常见的投诉形态。
type agentJoinRejection struct {
	UserID string `json:"userId"`
	Reason string `json:"reason"`
}

// agentJoinDeniedReason 是唯一一种拒绝原因：主人不允许该 agent 进群。
//
// 单独定义成常量而不是散落的字面量：前端要按它做文案映射（uniapp 那份还没
// 同步，取值一旦改写就会静默变成「加入失败」的兜底文案）。
const agentJoinDeniedReason = "permission_denied"

// decideAgentJoin 把候选成员拆成「能加」与「不能加」两拨，保持原顺序（添加
// 顺序会决定成员列表里的先后，重排会让用户以为勾选顺序没被尊重）。
//
// perms 里**没有**这个 id 表示它不是 agent 节点（真人好友），这类一律放行 ——
// 这个开关管的是智能体，把真人拦下来是纯粹的回归。
//
// inviterID 是发这条 addMember 的人（msg.FromUserID）。空串表示拿不到发起人：
// 此时**不做**主人豁免，照常按开关判。缺参数不是放行理由，fail-closed 优先
// （与 allows 的取舍一致）。
func decideAgentJoin(memberIDs []string, perms map[string]agentJoinPermission, inviterID string) (allowed []string, rejected []agentJoinRejection) {
	allowed = make([]string, 0, len(memberIDs))
	rejected = make([]agentJoinRejection, 0)
	inviterID = strings.TrimSpace(inviterID)

	for _, id := range memberIDs {
		perm, isAgent := perms[id]
		switch {
		case !isAgent:
			// 真人不归这个开关管。
			allowed = append(allowed, id)
		case perm.claimedBy == "":
			// 未认领：没有主人就没有可点的开关，按「关」处理会把整机主节点和
			// 没走认领流程的 agent 永久踢出所有群聊，而用户没有任何自救入口。
			allowed = append(allowed, id)
		case inviterID != "" && perm.claimedBy == inviterID:
			// 主人自己拉自己的 agent：开关约束的是别的账号。
			allowed = append(allowed, id)
		case perm.allowGroup:
			allowed = append(allowed, id)
		default:
			rejected = append(rejected, agentJoinRejection{UserID: id, Reason: agentJoinDeniedReason})
		}
	}
	return allowed, rejected
}

// agentJoinPermissionRows 是「候选成员的权限行」读取注入点。
//
// nil 时走数据库（defaultAgentJoinPermissionRows）。与 actorResolver 同一用途：
// 生产装配永远不设置它，测试用它免掉一个真库。
//
// 注入的是**行**而不是已经算好的 map：把注入点放在这里，测试才能同时覆盖
// 「行 → 判定输入」的映射（认领者 Valid/空串/空白三种形态）和「查询失败必须
// 往上抛」——如果注入的是结果，这两段都会退化成没人验的 I/O 尾巴，
// 把 `return nil, err` 改成 fail-open 也不会有一条用例变红。
//
// **生产路径不靠它，别把它当成开关。**
type agentJoinPermissionRows func(ctx context.Context, ids []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error)

// agentJoinPermissions 读候选成员的权限位。
//
// 返回的 map 只包含**确实存在节点行**的 id，调用方据此区分 agent 与真人。
func (h *systemHandler) agentJoinPermissions(ctx context.Context, ids []string) (map[string]agentJoinPermission, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := h.agentJoinPermissionRows(ctx, ids)
	if err != nil {
		// 往上抛，由调用方回 503。**不要**在这里吞成空 map —— 空 map 的含义是
		// 「这些人都是真人」，把一次查询故障翻译成「全员无限制」是最危险的退化。
		return nil, err
	}
	return joinPermissionsFromRows(rows), nil
}

// agentJoinPermissionRows 取行：注入了用注入的，否则查库。
func (h *systemHandler) agentJoinPermissionRows(ctx context.Context, ids []string) ([]db.ListRongCloudNodePermissionsByRcUserIDsRow, error) {
	if h.joinPermissionRows != nil {
		return h.joinPermissionRows(ctx, ids)
	}
	if h.queries == nil {
		// 没有 DB 的装配（单测 / 未接库的部署）里 rongcloud_node 整张表都不存在，
		// 也就不可能有「已认领的 agent」。返回空集等于「全员按真人放行」，
		// 与「查不到节点行」走同一分支 —— 不是静默放行，留一条日志好在排障时对上。
		h.logger.Debug("rongcloud: group join gate has no database, treating members as non-agents")
		return nil, nil
	}
	return h.queries.ListRongCloudNodePermissionsByRcUserIDs(ctx, ids)
}

// joinPermissionsFromRows 把权限行转成判定输入。
func joinPermissionsFromRows(rows []db.ListRongCloudNodePermissionsByRcUserIDsRow) map[string]agentJoinPermission {
	out := make(map[string]agentJoinPermission, len(rows))
	for _, row := range rows {
		// 只在 Valid 时取字符串：pgtype.Text 的零值是 {String:"",Valid:false}，
		// 但历史的 TEXT 列可能是空串而不是 NULL，两者都要当「没有主人」处理，
		// 所以统一 TrimSpace 后再判空。
		claimedBy := ""
		if row.ClaimedByUserID.Valid {
			claimedBy = strings.TrimSpace(row.ClaimedByUserID.String)
		}
		out[row.RongcloudUserID] = agentJoinPermission{
			claimedBy:  claimedBy,
			allowGroup: row.AllowGroup,
		}
	}
	return out
}
