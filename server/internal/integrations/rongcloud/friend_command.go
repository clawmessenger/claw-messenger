package rongcloud

// friend_command.go implements the service=friend command channel.
//
// 客户端的 9 个 action 见 clawmessenger-uniapp/src/utils/system-service-client.js:473-518：
//
//	getList        → 好友列表（页面读 res.data 或 res.data.list）
//	checkRelation  → {friendId} → {isFriend, isPending}
//	add            → {friendId, message, optType?}（optType 缺省 2 直加，1 = 发申请）
//	delete         → {friendId}
//	getDeviceOwner → {nodeId} → {ownerId}（AI 节点归属者）
//	sendRequest / getRequests / approveRequest / rejectRequest → 好友申请
//
// 好友关系的权威数据源是**融云好友边**，本服务不建好友关系表（调研结论见
// .agent-runs/2026-10-23/task-003/backend-im-service-pattern.md §3.3/§6.2）。
// 融云的 /friend/add.json 用 optType 区分两种行为：2 直加、1 发申请（user_api.go）。
// add 会把客户端传的 optType 透传下去（缺省仍 2，保持既有客户端的直加行为）；
// 但「申请」的读回与审批融云服务端 API 没有对等接口 —— getRequests /
// approveRequest / rejectRequest 统一回「暂不支持」的明确错误，而不是伪造一个
// 空列表：返回空数组会让「新的朋友」页以为真的没有申请（申请数据本身在融云
// SDK 侧，客户端有自己的能力）。sendRequest 仍列在 501 里只为保持四个 action
// 一致，发起申请请走 add + optType=1。
//
// 红线 #6（claw_admin_groups.go:263-265、node_service.go:1588-1603）：机器主节点
// 会被打成默认 ai_type，按 (machine_id, ai_type) 反查会命中它；好友/设备列表
// 必须排除它，否则新绑定的 agent 处处不可见。getList 用 isHiddenNode 过滤。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// friendUnsupportedMessage 是「好友申请」类 action 的固定文案。客户端会把
// message 直接弹给用户（search-user/index.vue），所以必须说清「缺的到底是
// 什么」：add 已经能发真申请（optType=1），真正没有的是**申请列表与审批**。
const friendUnsupportedMessage = "好友申请暂不支持：融云服务端未提供申请列表/审批接口，发起申请请用 friend/add 带 optType=1，读取与审批请用客户端融云 SDK 的好友申请能力"

// FriendStore is the local projection the friend service needs on top of the
// RongCloud friend edges. Sole callback today: inverting the edges to answer
// "这个 AI 节点归谁" (getDeviceOwner).
type FriendStore struct {
	// NodeOwner maps an agent node's RongCloud id to the claw account that
	// claims it. Nil disables getDeviceOwner only.
	NodeOwner func(ctx context.Context, nodeRCID string) (string, bool, error)
	// ReleaseNodeClaim 把「这条好友边已经没了」写回本地投影列
	// (rongcloud_node.claimed_by_user_id)。nil 表示未装配（不阻塞删除）。
	// ownerRCID 只用于「只清自己的那条边」，见 ClearNodeClaim。
	ReleaseNodeClaim func(ctx context.Context, nodeRCID, ownerRCID string) error
}

func (h *systemHandler) handleFriendCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.friendStore == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	switch cmd.Action {
	case "getList":
		return h.handleFriendList(ctx, msg, cmd)
	case "checkRelation":
		return h.handleFriendCheckRelation(ctx, msg, cmd)
	case "add":
		return h.handleFriendAdd(ctx, msg, cmd)
	case "delete":
		return h.handleFriendDelete(ctx, msg, cmd)
	case "getDeviceOwner":
		return h.handleFriendDeviceOwner(ctx, msg, cmd)
	case "sendRequest", "getRequests", "approveRequest", "rejectRequest":
		// 已注册但融云侧无对应能力：用 501 明确拒绝，绝不回空列表伪装成功。
		return h.replyErr(ctx, msg, cmd, 501, friendUnsupportedMessage)
	default:
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

func (h *systemHandler) handleFriendList(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	if h.client == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	friends, err := h.client.getFriends(ctx, userID)
	if err != nil {
		h.logger.Error("rongcloud: friend list", "error", err, "user_id", userID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the friend list")
	}
	records := make([]map[string]interface{}, 0, len(friends))
	for _, friend := range friends {
		// 内部节点（机器主节点 / ops 运维节点）不是「好友」，出现在列表里既
		// 暴露内部账号，也会让客户端把它们当可聊天对象。
		if h.isHiddenFriendNode(ctx, friend.UserID) {
			continue
		}
		records = append(records, h.friendRecord(ctx, friend))
	}
	// 页面两种读法都兼容：`Array.isArray(res.data) ? res.data : res.data.list`。
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"list":    records,
		"friends": records,
		"total":   len(records),
	})
}

func (h *systemHandler) handleFriendCheckRelation(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	friendID := strings.TrimSpace(getStringAny(cmd.Params, "friendId", "friend_id", "userId", "user_id"))
	if friendID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "friendId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	if h.client == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	ids, err := h.client.FriendIDs(ctx, userID)
	if err != nil {
		h.logger.Error("rongcloud: friend relation check", "error", err, "user_id", userID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to check the friend relation")
	}
	isFriend := false
	for _, id := range ids {
		if id == friendID {
			isFriend = true
			break
		}
	}
	// isPending 的真相只存在于融云 SDK 侧：服务端 API 能「发申请」
	// （/friend/add.json optType=1），却没有「读申请状态」的接口，所以这里
	// 查不出来。为了不骗客户端，回包里显式标 isPendingKnown=false：
	//   - isFriend=true  → isPending 必为 false 且是可信的（好友边已建）；
	//   - isFriend=false → isPending=false 只表示「好友边没建」，
	//     **不代表没有待处理申请**，客户端要判申请态必须用 SDK 的
	//     getFriendApplications / FRIEND_APPLICATION_STATUS_CHANGED。
	// 保留 isPending 键是为了兼容既有客户端（search-user/index.vue 用
	// `res.data?.isPending === true` 读，缺键也不会崩，但少一个字段更容易
	// 被误判成后端回退）。
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"friendId":       friendID,
		"isFriend":       isFriend,
		"isPending":      false,
		"isPendingKnown": isFriend,
	})
}

func (h *systemHandler) handleFriendAdd(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	friendID := strings.TrimSpace(getStringAny(cmd.Params, "friendId", "friend_id", "targetId", "target_id"))
	if friendID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "friendId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	if h.client == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	if friendID == userID {
		return h.replyErr(ctx, msg, cmd, 400, "cannot add yourself as a friend")
	}
	// optType 由客户端决定，缺省 2（直加）以保持既有客户端的行为不变；传 1
	// 时融云只发出好友申请，目标用户审批后才有好友边。
	optType := strings.TrimSpace(getStringAny(cmd.Params, "optType", "opt_type"))
	if optType == "" {
		optType = friendOptTypeDirect
	}
	if optType != friendOptTypeDirect && optType != friendOptTypeRequest {
		return h.replyErr(ctx, msg, cmd, 400, `optType must be "1" (request) or "2" (direct add)`)
	}
	// 客户端的 message 用作融云的备注名（remark）。
	remark := strings.TrimSpace(getStringAny(cmd.Params, "message", "remark"))
	alreadyFriends, err := h.client.addFriendWithOptType(ctx, userID, friendID, remark, optType)
	if err != nil {
		h.logger.Error("rongcloud: friend add", "error", err, "user_id", userID, "friend_id", friendID, "opt_type", optType)
		return h.replyErr(ctx, msg, cmd, 500, "failed to add the friend")
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"friendId":       friendID,
		"alreadyFriends": alreadyFriends,
		"optType":        optType,
		// optType=1 只是把申请发出去，好友边还没建 —— 客户端据此不能把对方
		// 当成好友渲染。
		"pending": optType == friendOptTypeRequest && !alreadyFriends,
	})
}

func (h *systemHandler) handleFriendDelete(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	friendID := strings.TrimSpace(getStringAny(cmd.Params, "friendId", "friend_id", "targetId", "target_id"))
	if friendID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "friendId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	if h.client == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	if err := h.client.RemoveFriends(ctx, userID, friendID); err != nil {
		h.logger.Error("rongcloud: friend delete", "error", err, "user_id", userID, "friend_id", friendID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to delete the friend")
	}
	// 写路径 ⑤：好友边删掉之后，本地投影列必须同步清空，否则 getDeviceOwner
	// 会一直答一个「已经不是好友」的账号 —— 脏列不像 15s 缓存那样会自己过期。
	// 只清「认领者正好是本次调用者」的那一行：任何人都能加/删别人的好友边，
	// 不能让 A 删掉 B 的边就把 B 的归属抹掉。
	if h.friendStore.ReleaseNodeClaim != nil {
		if err := h.friendStore.ReleaseNodeClaim(ctx, friendID, userID); err != nil {
			h.logger.Warn("rongcloud: release node claim after friend delete failed",
				"error", err, "user_id", userID, "friend_id", friendID)
		}
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"friendId": friendID,
		"deleted":  true,
	})
}

func (h *systemHandler) handleFriendDeviceOwner(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	nodeID := strings.TrimSpace(getStringAny(cmd.Params, "nodeId", "node_id", "rongcloudId", "userId", "user_id"))
	if nodeID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "nodeId is required")
	}
	// 归属查询不校验发送者：设备分享场景下调用方可能就是未登录的新用户，
	// 而这里返回的信息（谁拥有这台设备）本来就是分享卡片上公开的内容。
	if h.friendStore.NodeOwner == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	ownerID, found, err := h.friendStore.NodeOwner(ctx, nodeID)
	if err != nil {
		h.logger.Error("rongcloud: friend device owner", "error", err, "node_id", nodeID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to resolve the device owner")
	}
	if !found {
		return h.replyErr(ctx, msg, cmd, 404, "device owner not found")
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"nodeId":  nodeID,
		"ownerId": ownerID,
	})
}

// friendRecord enriches one RongCloud friend entry with the freshest local
// profile. 融云侧 name/portraitUri 可能落后（改资料后只有 refreshUser 推过去），
// 本地 claw_im_users / rongcloud_user 才是刚改完的那份。
func (h *systemHandler) friendRecord(ctx context.Context, friend friendEntry) map[string]interface{} {
	nickname := friend.Name
	portrait := friend.PortraitURI
	if h.queries != nil {
		if row, err := h.queries.GetClawIMUserByUserID(ctx, friend.UserID); err == nil {
			if name := stringOrNull(row.Nickname); name != "" {
				nickname = name
			} else if row.Username != "" {
				nickname = row.Username
			}
			if p := stringOrNull(row.PortraitUri); p != "" {
				portrait = p
			}
		} else if row, err := h.queries.GetRongCloudUserByRongCloudID(ctx, friend.UserID); err == nil {
			if row.Name.Valid && row.Name.String != "" {
				nickname = row.Name.String
			}
			if p := stringOrNull(row.PortraitUri); p != "" {
				portrait = p
			}
		}
	}
	if nickname == "" {
		nickname = friend.UserID
	}
	return map[string]interface{}{
		"userId":      friend.UserID,
		"id":          friend.UserID,
		"nickname":    nickname,
		"name":        nickname,
		"portraitUri": DefaultPortraitIfEmpty(portrait),
	}
}

// isHiddenFriendNode reports whether a friend id is one of the internal nodes
// that must never surface in a friend list (红线 #6). 查不到节点行是正常的
// （真人好友、安装账号都没有节点行），一律当作可见。
func (h *systemHandler) isHiddenFriendNode(ctx context.Context, rcID string) bool {
	if h.queries == nil || rcID == "" {
		return false
	}
	node, err := h.queries.GetRongCloudNodeByRongCloudUserID(ctx, rcID)
	if err != nil {
		return false
	}
	if isHiddenNode(node) {
		h.logger.Debug("rongcloud: hiding internal node from the friend list", "rongcloud_id", rcID)
		return true
	}
	return false
}

// NewFriendStore 是 FriendStore 的生产实现。NodeOwner 的真相是「融云好友边」的
// 反向映射（node_claim.go:18-35 的 NodeClaimants 同源语义）。为了不让每次
// getDeviceOwner 都把全表反查一遍，反查结果会被写进 rongcloud_node.
// claimed_by_user_id（600 号迁移）：
//
//  1. 读路径先查那一列（一次索引查询，O(1)）；
//  2. 列为空才降级走整表反查，并把结果惰性回填 —— 存量数据因此自愈，
//     不需要单独的 backfill 命令；
//  3. 所有好友边写路径（绑定 / 管理端转移 / 解绑 / 主持解绑 / 手删好友）
//     负责让这一列与边保持一致，见各写路径的注释。
func NewFriendStore(dbtx db.DBTX, client *rongcloudAPIClient, logger *slog.Logger) *FriendStore {
	if dbtx == nil || client == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	owners := &friendNodeOwners{dbtx: dbtx, client: client, logger: logger}
	return &FriendStore{NodeOwner: owners.lookup, ReleaseNodeClaim: owners.release}
}

// friendOwnerBuildTimeout bounds one whole-table inversion. It used to be one
// 10s call per account, serially — N accounts meant N×10s while the client gave
// up at 180s. Ownership is one line on a share card: give up early and let the
// next request retry rather than fan out indefinitely.
const friendOwnerBuildTimeout = 3 * time.Second

// friendOwnerBuildConcurrency caps the parallelism of the inversion so a big
// account table does not turn into a burst against the RongCloud API.
const friendOwnerBuildConcurrency = 8

type friendNodeOwners struct {
	dbtx   db.DBTX
	client *rongcloudAPIClient
	logger *slog.Logger

	// mu guards the memoised map below.
	mu      sync.Mutex
	cache   map[string]string
	cacheAt time.Time
	// epoch 是这张 map 建立时的 claimEpoch。写路径改列后世代号 +1，这里
	// 立刻判定过期 —— 这就是「解绑后不再答旧 owner」的实现（见 node_claim.go
	// 的 bumpClaimEpoch 说明）。
	epoch uint64
	// buildMu serialises build() so 10 concurrent lookups scan once, not 10
	// times (the map is written under mu, this only guards the expensive part).
	buildMu sync.Mutex
}

func (o *friendNodeOwners) lookup(ctx context.Context, nodeRCID string) (string, bool, error) {
	nodeRCID = strings.TrimSpace(nodeRCID)
	if nodeRCID == "" {
		return "", false, nil
	}
	// 1. 本地投影列：一次索引查询命中，热路径不再碰融云。
	if ownerID, ok := o.claimFromColumn(ctx, nodeRCID); ok {
		return ownerID, true, nil
	}
	// 2. 15s 记忆化的整表反查结果（同一页面加载里不重复扫）。
	if ownerID, found, hit := o.cachedLookup(nodeRCID); hit {
		return ownerID, found, nil
	}
	// 3. 降级：并发扫一遍好友边，总超时 friendOwnerBuildTimeout。
	//    buildEpoch 是这份 map 建立时的世代号，用来判断「build 期间有没有
	//    写路径改过列」—— 有的话，这份 map 可能来自解绑之前，绝不能落库。
	owners, buildEpoch, err := o.buildOnce(ctx)
	if err != nil {
		return "", false, err
	}
	ownerID, found := owners[nodeRCID]
	if !found {
		return "", false, nil
	}
	// 4. 惰性回填：存量行在第一次被访问时自愈。
	//
	//    但只在 build 期间没有写路径改列时才写：否则 build 带着解绑前的反查
	//    结果回来，而列此时正好被清成 NULL，`WHERE ... IS NULL` 仍然成立 →
	//    把旧 owner 写回去。列一旦非 NULL 就再也不会自愈（回填只补 NULL），
	//    「解绑后答旧 owner」会从 ≤15s 变成**永久**。宁可这一次不回填。
	if claimEpoch.Load() != buildEpoch {
		o.logger.Debug("rongcloud: skip node claim backfill, claim epoch moved during build",
			"node_rc", nodeRCID, "owner", ownerID)
		return ownerID, true, nil
	}
	o.backfill(ctx, nodeRCID, ownerID)
	return ownerID, true, nil
}

// claimFromColumn reads the local projection. Anything unexpected — a NULL
// value, a missing row, or a database that has not run migration 600 yet —
// means "unknown", which falls through to the authoritative inversion instead
// of failing the request.
func (o *friendNodeOwners) claimFromColumn(ctx context.Context, nodeRCID string) (string, bool) {
	var owner pgtype.Text
	err := o.dbtx.QueryRow(ctx,
		`SELECT claimed_by_user_id FROM rongcloud_node WHERE rongcloud_user_id = $1`,
		nodeRCID).Scan(&owner)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			o.logger.Debug("rongcloud: read node claim column failed",
				"node_rc", nodeRCID, "error", err)
		}
		return "", false
	}
	if owner.Valid && strings.TrimSpace(owner.String) != "" {
		return strings.TrimSpace(owner.String), true
	}
	return "", false
}

func (o *friendNodeOwners) cachedLookup(nodeRCID string) (string, bool, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.cacheFreshLocked() {
		return "", false, false
	}
	ownerID, found := o.cache[nodeRCID]
	return ownerID, found, true
}

// cacheFreshLocked 判定记忆化 map 是否还能用：未建 / 超过 TTL / 世代号落后
// （有写路径改过列）。调用方必须持有 o.mu。
func (o *friendNodeOwners) cacheFreshLocked() bool {
	if o.cache == nil || time.Since(o.cacheAt) >= claimCacheTTL {
		return false
	}
	return o.epoch == claimEpoch.Load()
}

// backfill writes an inversion result into the projection. WHERE ... IS NULL
// is deliberate: a non-NULL value is a write path's answer and must not be
// overwritten by a possibly older inversion.
func (o *friendNodeOwners) backfill(ctx context.Context, nodeRCID, ownerRCID string) {
	if ownerRCID = strings.TrimSpace(ownerRCID); ownerRCID == "" {
		return
	}
	tag, err := o.dbtx.Exec(ctx, `
		UPDATE rongcloud_node SET claimed_by_user_id = $1
		WHERE rongcloud_user_id = $2 AND claimed_by_user_id IS NULL`,
		ownerRCID, nodeRCID)
	if err != nil {
		o.logger.Debug("rongcloud: backfill node claim failed",
			"node_rc", nodeRCID, "owner", ownerRCID, "error", err)
		return
	}
	o.logger.Debug("rongcloud: node claim backfilled",
		"node_rc", nodeRCID, "owner", ownerRCID, "rows", tag.RowsAffected())
}

// release clears the projection after the friend edge was deleted (写路径 ⑤).
func (o *friendNodeOwners) release(ctx context.Context, nodeRCID, ownerRCID string) error {
	nodeRCID = strings.TrimSpace(nodeRCID)
	if nodeRCID == "" {
		return nil
	}
	if _, err := o.dbtx.Exec(ctx, `
		UPDATE rongcloud_node SET claimed_by_user_id = NULL
		WHERE rongcloud_user_id = $1 AND ($2 = '' OR claimed_by_user_id = $2)`,
		nodeRCID, strings.TrimSpace(ownerRCID)); err != nil {
		return err
	}
	// 写路径 ⑤ 改的是列，同样要让所有实例的反查 map 立刻作废。
	bumpClaimEpoch()
	return nil
}

// buildOnce runs build() at most once per cache window across concurrent
// callers: the first goroutine does the work, the rest wait and reuse its map.
//
// 第二个返回值是这份 map 对应的 claimEpoch：缓存命中时是缓存自己的世代号，
// 现建时是 build **之前**取的那个。调用方拿它和当前 epoch 比对，就能知道
// 「build 期间有没有写路径改过列」，从而决定敢不敢把结果回填进库。
func (o *friendNodeOwners) buildOnce(ctx context.Context) (map[string]string, uint64, error) {
	o.buildMu.Lock()
	defer o.buildMu.Unlock()

	o.mu.Lock()
	if o.cacheFreshLocked() {
		cached, at := o.cache, o.epoch
		o.mu.Unlock()
		return cached, at, nil
	}
	o.mu.Unlock()

	// 世代号在 build 之前取：如果 build 期间有写路径改列，epoch 会前进，
	// 这份 map 随后被判过期（宁可多重建一次，也不把旧结果当成新结果）。
	startEpoch := claimEpoch.Load()
	owners, err := o.build(ctx)
	if err != nil {
		return nil, startEpoch, err
	}
	o.mu.Lock()
	o.cache = owners
	o.cacheAt = time.Now()
	// 存的是 build 前的世代号，不是当前的：build 期间有写的话，下一次
	// cacheFreshLocked 立刻判过期并重建，不会把这批旧结果当成新结果复用。
	o.epoch = startEpoch
	o.mu.Unlock()
	return owners, startEpoch, nil
}

func (o *friendNodeOwners) build(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, friendOwnerBuildTimeout)
	defer cancel()

	rows, err := o.dbtx.Query(ctx, `SELECT user_id FROM claw_im_users WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list claw users for device owners: %w", err)
	}
	defer rows.Close()

	candidates := make([]string, 0, 64)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		if strings.TrimSpace(userID) != "" {
			candidates = append(candidates, userID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 并发反查：每个账号一次 /friend/get.json，最多 friendOwnerBuildConcurrency
	// 个在途。结果按候选下标落位，最后仍按候选顺序合并，让「首个写入者胜」
	// 与并发完成顺序无关（同一份数据每次渲染都一样）。
	type answer struct {
		friends []string
		err     error
	}
	answers := make([]answer, len(candidates))
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, friendOwnerBuildConcurrency)
	for i, userID := range candidates {
		i, userID := i, userID
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return nil
			}
			defer func() { <-sem }()
			friends, err := o.client.FriendIDs(gctx, userID)
			answers[i] = answer{friends: friends, err: err}
			return nil
		})
	}
	_ = g.Wait()

	owners := make(map[string]string, len(candidates))
	answered := 0
	var lastErr error
	for i, userID := range candidates {
		if answers[i].err != nil {
			// 单个账号失败只跳过：一个坏 token 不该让整张表失去归属。
			lastErr = answers[i].err
			continue
		}
		answered++
		for _, friend := range answers[i].friends {
			// 一条边可能被多个账号同时声明（理论上不该发生）；首个写入者胜，
			// 保证同一份数据每次渲染都一致。
			if _, taken := owners[friend]; !taken {
				owners[friend] = userID
			}
		}
	}
	if answered == 0 && lastErr != nil {
		return nil, lastErr
	}
	return owners, nil
}
