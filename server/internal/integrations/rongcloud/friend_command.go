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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
// 反向映射（node_claim.go:18-35 的 NodeClaimants 同源语义）：没有本地列记录
// 「谁认领了哪个节点」，只能把每个 C 端账号的好友列表翻过来。
//
// 代价是每个账号一次 /friend/get.json。与 NodeClaimants 一样按 claimCacheTTL
// 记忆化，避免一次页面加载里重复扫全表。单个账号查询失败只跳过（一个坏 token
// 不该让整张表失去归属）。
func NewFriendStore(dbtx db.DBTX, client *rongcloudAPIClient, logger *slog.Logger) *FriendStore {
	if dbtx == nil || client == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	owners := &friendNodeOwners{dbtx: dbtx, client: client, logger: logger}
	return &FriendStore{NodeOwner: owners.lookup}
}

type friendNodeOwners struct {
	dbtx   db.DBTX
	client *rongcloudAPIClient
	logger *slog.Logger

	mu      sync.Mutex
	cache   map[string]string
	cacheAt time.Time
}

func (o *friendNodeOwners) lookup(ctx context.Context, nodeRCID string) (string, bool, error) {
	o.mu.Lock()
	if o.cache != nil && time.Since(o.cacheAt) < claimCacheTTL {
		ownerID, found := o.cache[nodeRCID]
		o.mu.Unlock()
		return ownerID, found, nil
	}
	o.mu.Unlock()

	owners, err := o.build(ctx)
	if err != nil {
		return "", false, err
	}
	o.mu.Lock()
	o.cache = owners
	o.cacheAt = time.Now()
	o.mu.Unlock()

	ownerID, found := owners[nodeRCID]
	return ownerID, found, nil
}

func (o *friendNodeOwners) build(ctx context.Context) (map[string]string, error) {
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

	owners := make(map[string]string, len(candidates))
	answered := 0
	var lastErr error
	for _, userID := range candidates {
		friends, err := o.client.FriendIDs(ctx, userID)
		if err != nil {
			lastErr = err
			continue
		}
		answered++
		for _, friend := range friends {
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
