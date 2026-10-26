package rongcloud

// friend_request.go implements 「加 AI 节点为好友，由节点拥有者审批」。
//
// 为什么需要这张表而不是只靠融云：融云服务端 API 能**发出**好友申请
// （/friend/add.json optType=1），但没有「读申请列表 / 同意 / 拒绝」的对等
// 接口（user_api.go:72-120 只有 add/get/remove）。所以「谁在申请哪台设备、
// 拥有者批没批」只能自己存（migrations/603_im_friend_request）。
//
// 这条链路补的是移动端那套降级实现缺的一步：扫码加节点时申请只能发给**拥有者
// 账号**，拥有者在融云「新的朋友」里同意后建立的是**人↔人**好友边，请求方
// 拿不到那个 AI 节点 —— 节点好友边原本只由绑定/认领流程写给拥有者自己
// （node_service.go:1403）。审批通过时这里补建「请求方↔节点」双向边。
//
// 审批入口只有一个：本服务的 approveRequest（web 审批 UI 用它，能拿到结构化的
// deviceNodeId）。拥有者直接在融云「新的朋友」里点同意这条路**本轮不自动补边**，
// 因为它的判据不可信：
//  1. 融云没有好友事件 webhook（webhook.go 只解析消息），服务端分不清「拥有者
//     同意了」和「请求方自己造了一条边」；
//  2. friend/add 的 optType 由客户端决定且缺省 2（直加，friend_command.go），
//     请求方可以单方面把自己与拥有者变成「好友」—— 「当前是好友」这个事实
//     根本不能当审批凭据。早先按它做读路径对账，等于把审批权交给了请求方：
//     他只要调一次 checkRelation，服务端就会替他把节点边建好。
// 想在融云侧审批的客户端（移动端）应在 acceptFriendApplication 成功后显式调
// friend/approveRequest，而不是指望服务端从好友边反推。
//
// 去掉对账不会留下死状态：拥有者先在融云侧同意过、之后再来 web 点同意，仍然走
// completeFriendRequest —— 融云对已存在的边回 25460，按成功处理（见
// addFriendWithOptType），所以「先私下同意、再在 web 补批」是幂等可行的。
//
// 权限红线：审批人只能是申请里记录的 to_user_id（不可由客户端指定），且每次
// 审批前重新复核设备当前拥有者。
//
// 设备所有权转移时，`im_friend_request` 里该设备的**待审批**行会随拥有者变更被作废
// （approve 时复核 owner 不一致即拒），但**已批准的历史行不会被改写** —— 它记录的是
// 「当时的拥有者同意过」。因此判定「是否已批准」必须带上 `to_user_id`（见
// HasApproved），否则新拥有者的拒绝会因为上一个拥有者的旧批准而失效。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	friendRequestPending  int16 = 0
	friendRequestApproved int16 = 1
	friendRequestRejected int16 = 2

	// friendRequestListLimit 是「新的朋友」一页的上限。拥有者同时挂着的申请
	// 正常只有个位数，这里只防一条脏数据把回包撑爆。
	friendRequestListLimit = 50
)

// errFriendRequestOwnerChanged 表示审批时设备拥有者已经换了人：这条申请
// 不能再生效（否则等于让旧拥有者替新拥有者授权节点）。
var errFriendRequestOwnerChanged = errors.New("rongcloud: device owner changed")

// FriendRequest 是一条「请求把某台 AI 节点加为好友」的申请。
//
// FromUserID / ToUserID 都是 claw_im_users.user_id（同时也是融云 user id）；
// DeviceNodeID 是节点的融云 id —— 审批通过后据此建好友边。
type FriendRequest struct {
	ID           string
	FromUserID   string
	ToUserID     string
	DeviceNodeID string
	Message      string
	Status       int16
	CreatedAt    time.Time
}

// FriendRequestStore 是 im_friend_request 的读写口。
//
// 与 FriendStore 同款做法：暴露函数字段，测试直接注入内存实现，不依赖数据库
// （见 friend_device_owner_claim_test.go 的 claimStoreDB 范式）。
type FriendRequestStore struct {
	// Create 幂等落一条待审批申请：同一请求方对同一设备已存在待审批行时只更新
	// 备注与审批人，不新增行（部分唯一索引 uq_im_friend_request_pending）。
	Create func(ctx context.Context, req FriendRequest) (FriendRequest, error)
	// FindPending 查「这个请求方对这台设备有没有待审批申请」。
	FindPending func(ctx context.Context, fromUserID, deviceNodeID string) (FriendRequest, bool, error)
	// Get 按 id 取一条（审批前校验申请人与状态用）。
	Get func(ctx context.Context, id string) (FriendRequest, bool, error)
	// ListPending 列某人的待审批申请（他作为审批人）。
	ListPending func(ctx context.Context, toUserID string) ([]FriendRequest, error)
	// Decide 把待审批行改成终态；toUserID 必须与行上的 to_user_id 一致，
	// 否则改不动（返回 false）。已不是待审批时同样返回 false，调用方据此
	// 区分「幂等重放」与「越权」。
	Decide func(ctx context.Context, id, toUserID string, status int16) (FriendRequest, bool, error)
	// HasApproved 判断「当前拥有者」是否批准过这个请求方加这台设备的申请。
	// 必须带 toUserID：设备可能换过主人，上一个拥有者的批准不能代表新拥有者的意思。
	HasApproved func(ctx context.Context, fromUserID, deviceNodeID, toUserID string) (bool, error)
}

// friendRequestColumns 固定选择列顺序，避免 scan 与 SQL 顺序漂移。
// decided_at / decided_by 刻意不读：API 不返回它们，也就不必处理 NULL 扫描。
const friendRequestColumns = `id::text, from_user_id, to_user_id, device_node_id, message, status, created_at`

func scanFriendRequest(row pgx.Row) (FriendRequest, error) {
	var req FriendRequest
	err := row.Scan(
		&req.ID, &req.FromUserID, &req.ToUserID, &req.DeviceNodeID,
		&req.Message, &req.Status, &req.CreatedAt,
	)
	if err != nil {
		return FriendRequest{}, err
	}
	return req, nil
}

// NewFriendRequestStore 用裸参数化 SQL 实现（不走 sqlc）：603 是新增表，
// generated/ 目录本轮不做全量 regen（工作区里有在途改动），与 migrations/600
// 的 claimed_by_user_id 同一处理方式（node_claim.go:137-141）。
func NewFriendRequestStore(dbtx db.DBTX, logger *slog.Logger) *FriendRequestStore {
	if dbtx == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FriendRequestStore{
		Create: func(ctx context.Context, req FriendRequest) (FriendRequest, error) {
			// 冲突目标带 WHERE status = 0，才能命中那个部分唯一索引。
			row := dbtx.QueryRow(ctx, `
				INSERT INTO im_friend_request (from_user_id, to_user_id, device_node_id, message)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (from_user_id, device_node_id) WHERE status = 0
				DO UPDATE SET message = EXCLUDED.message,
				              to_user_id = EXCLUDED.to_user_id,
				              updated_at = now()
				RETURNING `+friendRequestColumns,
				req.FromUserID, req.ToUserID, req.DeviceNodeID, req.Message)
			return scanFriendRequest(row)
		},
		FindPending: func(ctx context.Context, fromUserID, deviceNodeID string) (FriendRequest, bool, error) {
			row := dbtx.QueryRow(ctx, `
				SELECT `+friendRequestColumns+`
				FROM im_friend_request
				WHERE from_user_id = $1 AND device_node_id = $2 AND status = 0
				ORDER BY created_at DESC
				LIMIT 1`, fromUserID, deviceNodeID)
			req, err := scanFriendRequest(row)
			if errors.Is(err, pgx.ErrNoRows) {
				return FriendRequest{}, false, nil
			}
			if err != nil {
				return FriendRequest{}, false, err
			}
			return req, true, nil
		},
		Get: func(ctx context.Context, id string) (FriendRequest, bool, error) {
			row := dbtx.QueryRow(ctx, `
				SELECT `+friendRequestColumns+`
				FROM im_friend_request
				WHERE id = $1::uuid`, id)
			req, err := scanFriendRequest(row)
			if errors.Is(err, pgx.ErrNoRows) {
				return FriendRequest{}, false, nil
			}
			if err != nil {
				return FriendRequest{}, false, err
			}
			return req, true, nil
		},
		ListPending: func(ctx context.Context, toUserID string) ([]FriendRequest, error) {
			rows, err := dbtx.Query(ctx, `
				SELECT `+friendRequestColumns+`
				FROM im_friend_request
				WHERE to_user_id = $1 AND status = 0
				ORDER BY created_at DESC
				LIMIT $2`, toUserID, friendRequestListLimit)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			requests := make([]FriendRequest, 0, 8)
			for rows.Next() {
				req, scanErr := scanFriendRequest(rows)
				if scanErr != nil {
					return nil, scanErr
				}
				requests = append(requests, req)
			}
			return requests, rows.Err()
		},
		Decide: func(ctx context.Context, id, toUserID string, status int16) (FriendRequest, bool, error) {
			if status != friendRequestApproved && status != friendRequestRejected {
				return FriendRequest{}, false, fmt.Errorf("rongcloud: unsupported friend request status %d", status)
			}
			row := dbtx.QueryRow(ctx, `
				UPDATE im_friend_request
				SET status = $3, decided_at = now(), decided_by = $2, updated_at = now()
				WHERE id = $1::uuid AND to_user_id = $2 AND status = 0
				RETURNING `+friendRequestColumns, id, toUserID, status)
			req, err := scanFriendRequest(row)
			if errors.Is(err, pgx.ErrNoRows) {
				return FriendRequest{}, false, nil
			}
			if err != nil {
				return FriendRequest{}, false, err
			}
			return req, true, nil
		},
		HasApproved: func(ctx context.Context, fromUserID, deviceNodeID, toUserID string) (bool, error) {
			// EXISTS 而不是 COUNT：只要有过一条已通过的行就算（同一对可能有多条
			// 历史行，重复申请会各留一条）。
			// 必须带 to_user_id：设备换过主人时，上一个拥有者的批准不能代表新拥有者
			// 的意思 —— 少了这个条件，新拥有者的拒绝会因为旧批准而失效。
			var exists bool
			if err := dbtx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM im_friend_request
					WHERE from_user_id = $1 AND device_node_id = $2 AND to_user_id = $3 AND status = 1
				)`, fromUserID, deviceNodeID, toUserID).Scan(&exists); err != nil {
				return false, err
			}
			return exists, nil
		},
	}
}

// ---- 命令通道 ----

// handleFriendSendRequest 是 friend/sendRequest：发起「加某台 AI 节点为好友」。
//
// 参数：deviceNodeId（必填）、message（可选备注）、toUserId（可选，只用于校验）。
// 回包：{requestId, status:'pending', ownerId, delivered}。
// 已经是好友时不落行、不投递，直接回 {status:'alreadyFriend', ownerId, nodeId}
// （没有 requestId —— 调用方据此知道这次没有产生待审批申请）。
func (h *systemHandler) handleFriendSendRequest(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if !h.friendApprovalReady() {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	actor, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	deviceNodeID := strings.TrimSpace(getStringAny(cmd.Params, "deviceNodeId", "device_node_id", "nodeId", "node_id"))
	if deviceNodeID == "" {
		// 普通用户之间的好友申请走 friend/add 带 optType=1（融云原生申请）。
		// 这里是节点专属通道：没有节点就没有「谁审批、批完建哪条边」。
		return h.replyErr(ctx, msg, cmd, 400, "deviceNodeId is required: 普通好友申请请用 friend/add 带 optType=1")
	}
	if err := h.ensureVisibleAgentNode(ctx, deviceNodeID); err != nil {
		return h.replyErrorForNode(ctx, msg, cmd, err)
	}
	ownerID, found, err := h.friendStore.NodeOwner(ctx, deviceNodeID)
	if err != nil {
		h.logger.Error("rongcloud: friend request resolve owner", "error", err, "node", deviceNodeID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to resolve the device owner")
	}
	if !found || ownerID == "" {
		return h.replyErr(ctx, msg, cmd, 404, "device owner not found")
	}
	if ownerID == actor {
		return h.replyErr(ctx, msg, cmd, 400, "cannot add your own device as a friend")
	}
	// 审批人只能是真实拥有者。客户端可以带 toUserId，但只用来暴露「你看到的
	// 拥有者已经过期」，绝不允许指定别人代批。
	if claimed := strings.TrimSpace(getStringAny(cmd.Params, "toUserId", "to_user_id")); claimed != "" && claimed != ownerID {
		return h.replyErr(ctx, msg, cmd, 409, "device owner changed, please retry")
	}
	remark := strings.TrimSpace(getStringAny(cmd.Params, "message", "remark"))
	// 已经是好友就直接回 alreadyFriend，不落行、也不再向拥有者投递申请。
	//
	// Create 的唯一索引只约束 status = 0（见 603 的 uq_im_friend_request_pending），
	// 所以「批过之后再点一次申请」会新建一条 pending 行；拥有者随后点拒绝，就会
	// 走到 reject 的撤边逻辑 —— 一条重复的 pending 被拒，不该毁掉已经生效的节点
	// 关系。这是普通操作可达的路径，不是竞态，必须在入口挡住。
	// （reject 那边还有 HasApproved 兜底，这里挡住是为了连行和融云申请都不产生。）
	if ids, err := h.client.FriendIDs(ctx, actor); err != nil {
		// 好友列表读不到只意味着少了一次短路：Create 幂等，reject 又有兜底，
		// 不值得为此让「发申请」整体失败。
		h.logger.Warn("rongcloud: friend request pre-check friend list failed",
			"error", err, "from", actor, "node", deviceNodeID)
	} else if slices.Contains(ids, deviceNodeID) {
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{
			"status":  "alreadyFriend",
			"ownerId": ownerID,
			"nodeId":  deviceNodeID,
		})
	}
	request, err := h.friendRequests.Create(ctx, FriendRequest{
		FromUserID:   actor,
		ToUserID:     ownerID,
		DeviceNodeID: deviceNodeID,
		Message:      remark,
	})
	if err != nil {
		h.logger.Error("rongcloud: friend request create", "error", err, "from", actor, "node", deviceNodeID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to create the friend request")
	}
	// 同时投递到融云侧：拥有者在「新的朋友」里能看到并直接同意（移动端现状）。
	// 投递失败不影响审批本身 —— 申请行已经是真相，拥有者在 web 也能批。
	delivered := true
	if _, err := h.client.addFriendWithOptType(
		ctx, actor, ownerID, h.friendRequestRemark(ctx, remark, deviceNodeID), friendOptTypeRequest,
	); err != nil {
		delivered = false
		h.logger.Warn("rongcloud: friend request delivery to rongcloud failed",
			"error", err, "from", actor, "owner", ownerID, "node", deviceNodeID)
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"requestId": request.ID,
		"status":    "pending",
		"ownerId":   ownerID,
		"nodeId":    deviceNodeID,
		// delivered=false 表示融云侧申请没投递成功（拥有者在「新的朋友」里看不到），
		// 但申请行已落库，web 的审批列表仍能看到。
		"delivered": delivered,
	})
}

// handleFriendGetRequests 是 friend/getRequests：列我作为审批人的待审批申请。
func (h *systemHandler) handleFriendGetRequests(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.friendRequests == nil {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	// 身份只认发送者。客户端会带 userId，但那是兼容字段，不能作为查询条件 ——
	// 否则任何人都能读到别人的申请列表。
	actor, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	requests, err := h.friendRequests.ListPending(ctx, actor)
	if err != nil {
		h.logger.Error("rongcloud: list friend requests", "error", err, "to", actor)
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the friend requests")
	}
	list := make([]map[string]interface{}, 0, len(requests))
	for _, request := range requests {
		// 只信 im_friend_request 这一张表。不做「拥有者是不是已经在融云侧
		// 同意过」的读路径对账：那个判据可以被请求方单方面造出来（见文件头
		// 注释的两条理由），会给待审批列表开出自动批准的后门。
		list = append(list, h.friendRequestRecord(ctx, request))
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"list": list})
}

// handleFriendApproveRequest 是 friend/approveRequest：拥有者同意。
func (h *systemHandler) handleFriendApproveRequest(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	return h.decideFriendRequest(ctx, msg, cmd, friendRequestApproved)
}

// handleFriendRejectRequest 是 friend/rejectRequest：拥有者拒绝。
func (h *systemHandler) handleFriendRejectRequest(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	return h.decideFriendRequest(ctx, msg, cmd, friendRequestRejected)
}

func (h *systemHandler) decideFriendRequest(ctx context.Context, msg NormalizedMessage, cmd CommandContent, status int16) error {
	if !h.friendApprovalReady() {
		return h.replyErr(ctx, msg, cmd, 503, "friend service is unavailable")
	}
	actor, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	requestID := strings.TrimSpace(getStringAny(cmd.Params, "requestId", "request_id", "id"))
	if _, err := parseUUIDParam(requestID); err != nil {
		return h.replyErr(ctx, msg, cmd, 400, "invalid requestId")
	}
	request, found, err := h.friendRequests.Get(ctx, requestID)
	if err != nil {
		h.logger.Error("rongcloud: load friend request", "error", err, "request", requestID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the friend request")
	}
	if !found {
		return h.replyErr(ctx, msg, cmd, 404, "friend request not found")
	}
	if request.ToUserID != actor {
		// 只有被申请的拥有者能批。这条是审批链路的权限边界，不能用 404 掩盖
		// 成「不存在」—— 前端要能提示「你不是这台设备的拥有者」。
		return h.replyErr(ctx, msg, cmd, 403, "only the device owner can decide this request")
	}
	if request.Status != friendRequestPending {
		if request.Status == status {
			// 幂等重放：已经批过/拒过，按成功回，避免客户端重试变成报错。
			return h.replyOK(ctx, msg, cmd, h.friendDecisionPayload(ctx, request))
		}
		return h.replyErr(ctx, msg, cmd, 409, "friend request already decided")
	}
	if status == friendRequestRejected {
		decided, ok, err := h.friendRequests.Decide(ctx, request.ID, actor, friendRequestRejected)
		if err != nil {
			h.logger.Error("rongcloud: reject friend request", "error", err, "request", request.ID)
			return h.replyErr(ctx, msg, cmd, 500, "failed to reject the friend request")
		}
		if !ok {
			return h.replyErr(ctx, msg, cmd, 409, "friend request already decided")
		}
		// 拒绝必须顺带撤销可能已经建出来的节点边：approve 是逐条建的，中途失败
		// 会留下单向边（请求方→节点已建、反向失败），此时行仍是待审批。不撤的话
		// 请求方好友列表里已经能看到这台节点，却从来没被批准过。
		// 拒绝本身已经生效，撤边失败只告警（见 removeNodeEdges）。
		// 用带审批兜底的版本：这对 (请求方, 设备) 若已有过通过的申请（重复申请
		// 或历史脏数据），撤边会连已经生效的关系一起删掉。
		h.revokeNodeEdgesUnlessApproved(ctx, request.FromUserID, request.DeviceNodeID, request.ToUserID)
		return h.replyOK(ctx, msg, cmd, h.friendDecisionPayload(ctx, decided))
	}
	approved, err := h.completeFriendRequest(ctx, request)
	if err != nil {
		if errors.Is(err, errFriendRequestOwnerChanged) {
			return h.replyErr(ctx, msg, cmd, 409, "device owner changed, this request is no longer valid")
		}
		h.logger.Error("rongcloud: approve friend request", "error", err, "request", request.ID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to approve the friend request")
	}
	if approved.Status != friendRequestApproved {
		// 并发下被另一条路径先拒绝了（completeFriendRequest 读到的是库里的终态）：
		// 这里绝不能报成功，否则「拒绝」会被回成「通过」。
		return h.replyErr(ctx, msg, cmd, 409, "friend request already decided")
	}
	return h.replyOK(ctx, msg, cmd, h.friendDecisionPayload(ctx, approved))
}

// completeFriendRequest 是审批链路的唯一收尾点：复核拥有者 → 建三条好友边 →
// 复核拥有者 → 置为已通过。幂等，且**先建边后改状态**：边没建成就保持待审批，
// 重试还能补上（反过来会留下「已通过但节点边不存在」的死状态）。
func (h *systemHandler) completeFriendRequest(ctx context.Context, request FriendRequest) (FriendRequest, error) {
	ownerID, found, err := h.friendStore.NodeOwner(ctx, request.DeviceNodeID)
	if err != nil {
		return request, err
	}
	if !found || ownerID == "" || ownerID != request.ToUserID {
		// 设备换了主人：这条历史申请作废（拒绝掉，别再挂着）。
		_, _, _ = h.friendRequests.Decide(ctx, request.ID, request.ToUserID, friendRequestRejected)
		return request, errFriendRequestOwnerChanged
	}
	// 1) 拥有者 ↔ 请求方：让两人成为好友（若不存在）。
	//    拥有者在融云侧同意过、之后再来 web 补批时，这条边通常已存在；
	//    融云回 25460 时 addFriendWithOptType 会按 alreadyFriends 成功返回。
	//
	//    这条边**是不是本来就存在**决定了收尾能不能撤它：请求方与拥有者原本就是
	//    好友（与本次申请无关）时，撤掉等于替两个用户删好友。所以只在「本次真的
	//    新建了这条边」时才允许撤；建边本身报错时也不知道原状态，一律当「不撤」。
	removeOwnerEdge := false
	if ownerEdgeAlready, edgeErr := h.client.addFriendWithOptType(ctx, ownerID, request.FromUserID, "", friendOptTypeDirect); edgeErr != nil {
		h.logger.Warn("rongcloud: approve request owner edge failed",
			"error", edgeErr, "owner", ownerID, "from", request.FromUserID)
	} else {
		removeOwnerEdge = !ownerEdgeAlready
	}
	// 2) 请求方 ↔ 节点 双向边 —— 这才是「把智能体加为好友」的实体。
	if _, err := h.client.addFriendWithOptType(ctx, request.FromUserID, request.DeviceNodeID, "", friendOptTypeDirect); err != nil {
		return request, fmt.Errorf("requester to node edge: %w", err)
	}
	if _, err := h.client.addFriendWithOptType(ctx, request.DeviceNodeID, request.FromUserID, "", friendOptTypeDirect); err != nil {
		return request, fmt.Errorf("node to requester edge: %w", err)
	}
	// 3) 落状态之前再复核一次拥有者：上面那次检查与建边之间没有任何事务/锁保护，
	//    设备在这中间被转移的话，边已经建给了旧主人批准的对象。这时必须撤销刚建的
	//    边并把申请拒掉，否则新拥有者名下会凭空多出一个「好友」。
	latestOwner, stillFound, ownerErr := h.friendStore.NodeOwner(ctx, request.DeviceNodeID)
	if ownerErr != nil {
		return request, ownerErr
	}
	if !stillFound || latestOwner != request.ToUserID {
		h.revokeApprovedEdges(ctx, ownerID, request.FromUserID, request.DeviceNodeID, removeOwnerEdge)
		_, _, _ = h.friendRequests.Decide(ctx, request.ID, request.ToUserID, friendRequestRejected)
		return request, errFriendRequestOwnerChanged
	}
	decided, ok, err := h.friendRequests.Decide(ctx, request.ID, ownerID, friendRequestApproved)
	if err != nil {
		return request, err
	}
	if !ok {
		// 并发下被另一条路径先批/先拒了：读出当前状态，按同一结果返回
		// （调用方据 status 判断是「幂等重放」还是「已经被人拒绝」）。
		current, found, getErr := h.friendRequests.Get(ctx, request.ID)
		if getErr == nil && found {
			// 上面那几条边已经建好了，而这条申请是被别人拒绝掉的：不撤就等于「一条
			// 被拒绝的申请仍然拿到了节点」。这里必须回滚。
			//
			// 只撤节点边：拥有者↔请求方 那条不是审批边界（friend/add 缺省
			// optType=2，任何人本来就能单方面和任何人成为融云好友），撤它得不到
			// 安全性，反而可能删掉本来就存在的真人好友关系。
			// 撤边前同样过一遍 HasApproved：这对关系若早有一条已通过的申请，
			// 刚建的节点边是那条已批准关系的（重复申请），不该被这条被拒的行牵走。
			if current.Status == friendRequestRejected {
				h.revokeNodeEdgesUnlessApproved(ctx, request.FromUserID, request.DeviceNodeID, request.ToUserID)
			}
			return current, nil
		}
		return request, nil
	}
	return decided, nil
}

// removeNodeEdges best-effort 撤销「请求方 ↔ 节点」双向边。
//
// 只告警不报错：调用方的决策（拒绝 / 作废）已经生效，撤边失败不该把一次成功的
// 拒绝回成失败。残留的边由下一次 approve/reject 或删好友兜底清理。
//
// 两个方向各自独立处理：第一个方向失败不能跳过第二个。
//
// 关于日志噪音：reject 与「approve 输掉并发」的回滚都会调用这里（前者经
// revokeNodeEdgesUnlessApproved），所以「本来就没有边」也会走一次。
// RongCloud 的 /friend/delete.json 对不存在的边大概率回非 200，而
// rongcloudAPIClient.RemoveFriends 只把 postForm 的错误原样返回（
// "rongcloud: API error code N: ..."），**拿不到可区分「不是好友」的结构化码**
// —— 要区分就得改 RemoveFriends 的签名或去做错误字符串匹配，都不值得。
// 因此这里的 Warn **属于预期噪音，不要据此判断撤边失败**；真正的撤边失败只有
// 结合融云侧好友列表才能确认。
func (h *systemHandler) removeNodeEdges(ctx context.Context, fromUserID, deviceNodeID string) {
	if strings.TrimSpace(fromUserID) == "" || strings.TrimSpace(deviceNodeID) == "" {
		return
	}
	for _, edge := range [][2]string{
		{fromUserID, deviceNodeID},
		{deviceNodeID, fromUserID},
	} {
		if err := h.client.RemoveFriends(ctx, edge[0], edge[1]); err != nil {
			h.logger.Warn("rongcloud: revoke node friend edge failed",
				"error", err, "user", edge[0], "target", edge[1])
		}
	}
}

// revokeNodeEdgesUnlessApproved 在「**当前拥有者**没有批准过这个请求方加这台设备」
// 时才撤节点边。
//
// 为什么需要兜底：Create 的唯一索引只约束 status = 0，所以一条重复的 pending 行
// 可以存在于一对**已经批准过**的关系之上（用户批完再加一次、或历史脏数据）。
// 拥有者点拒绝时若无条件撤边，就把一条已生效的节点关系删掉了 —— 这是普通操作
// 可达的，不是竞态。sendRequest 入口已经挡住「已是好友再申请」，这里是第二道，
// 防的是并发路径与存量数据。
//
// toUserID 必须是**当前拥有者**（调用点传 request.ToUserID：reject 路径在签发前已
// 校验它等于 actor，approve 回滚路径刚复核过 latestOwner == request.ToUserID）。
// 不带这个条件的话，设备换过主人时会用「上一个拥有者的旧批准」把新拥有者的拒绝
// 挡掉 —— 接口回 200、界面显示已拒绝，请求方却仍把这台节点挂在好友列表里。
//
// HasApproved 本身出错时**跳过撤边**：宁可不撤，也不能误删已生效的关系。
func (h *systemHandler) revokeNodeEdgesUnlessApproved(ctx context.Context, fromUserID, deviceNodeID, toUserID string) {
	if h.friendRequests != nil && h.friendRequests.HasApproved != nil {
		approved, err := h.friendRequests.HasApproved(ctx, fromUserID, deviceNodeID, toUserID)
		if err != nil {
			h.logger.Warn("rongcloud: check an approved friend request before revoking edges failed",
				"error", err, "from", fromUserID, "node", deviceNodeID, "owner", toUserID)
			return
		}
		if approved {
			h.logger.Debug("rongcloud: keep node edges, the current owner approved this pair",
				"from", fromUserID, "node", deviceNodeID, "owner", toUserID)
			return
		}
	}
	h.removeNodeEdges(ctx, fromUserID, deviceNodeID)
}

// revokeApprovedEdges 撤销 approve 刚建出的边（拥有者↔请求方 + 节点双向边），
// 用于「建边期间设备换了主人」的收尾。同样是 best-effort。
//
// removeOwnerEdge 只在那条边**由本次 approve 建出**时为 true：请求方与拥有者本来
// 就是好友时（addFriendWithOptType 回 alreadyFriends，或建边报错不知道原状态），
// 撤掉它等于替两个用户删好友。节点那两条边不用区分，它们只可能由本次申请建出。
func (h *systemHandler) revokeApprovedEdges(ctx context.Context, ownerID, fromUserID, deviceNodeID string, removeOwnerEdge bool) {
	if removeOwnerEdge && strings.TrimSpace(ownerID) != "" && strings.TrimSpace(fromUserID) != "" {
		if err := h.client.RemoveFriends(ctx, ownerID, fromUserID); err != nil {
			h.logger.Warn("rongcloud: revoke owner edge failed",
				"error", err, "owner", ownerID, "from", fromUserID)
		}
	}
	h.removeNodeEdges(ctx, fromUserID, deviceNodeID)
}

// ---- 回包与辅助 ----

// friendApprovalReady 判断审批链路的三件依赖是否都装配到位：审批表、好友边
// 归属解析（NodeOwner）、融云客户端。缺一件就回 503 —— 比 panic 或「只做一半」
// 诚实（例如没有 client 时批了状态却建不出边，就会留下「已通过但加不上」）。
func (h *systemHandler) friendApprovalReady() bool {
	return h.friendRequests != nil &&
		h.friendStore != nil &&
		h.friendStore.NodeOwner != nil &&
		h.client != nil
}

// friendDecisionPayload 把一行申请转成审批回包。
//
// 状态**只取库里的那一行**（friendRequestRecord 已经把它写进 status），不接受
// 调用方意图覆盖：approve 撞上并发 reject 时，按意图覆盖会把「已拒绝」报成
// 「已通过」，前端随后会把一个并没有建边成功的节点当成好友。
func (h *systemHandler) friendDecisionPayload(ctx context.Context, request FriendRequest) map[string]interface{} {
	payload := h.friendRequestRecord(ctx, request)
	payload["approved"] = request.Status == friendRequestApproved
	return payload
}

// friendRequestRecord 把一行申请转成前端契约（web NodeFriendApplication /
// 移动端申请卡片共用）。
func (h *systemHandler) friendRequestRecord(ctx context.Context, request FriendRequest) map[string]interface{} {
	record := map[string]interface{}{
		"id":           request.ID,
		"fromUserId":   request.FromUserID,
		"fromNickname": request.FromUserID,
		"toUserId":     request.ToUserID,
		"message":      request.Message,
		"status":       request.Status,
		"deviceNodeId": request.DeviceNodeID,
		"deviceName":   h.nodeDisplayName(ctx, request.DeviceNodeID),
		"createdAt":    request.CreatedAt.UTC().Format(time.RFC3339),
	}
	if h.queries != nil {
		if user, err := h.queries.GetRongCloudUserByRongCloudID(ctx, request.FromUserID); err == nil {
			if user.Name.Valid && strings.TrimSpace(user.Name.String) != "" {
				record["fromNickname"] = user.Name.String
			}
			if user.PortraitUri.Valid && strings.TrimSpace(user.PortraitUri.String) != "" {
				record["fromPortraitUri"] = user.PortraitUri.String
			}
		}
		// 昵称还是那串 id，说明镜像表里要么没这号人（C 端真人只写
		// claw_im_users，clawRong 直接调融云 API 不写镜像），要么没填名字。
		// 这时退到 claw_im_users 再取一次，否则申请卡片上只显示一串
		// claw_xxx / user-xxx，拥有者根本不知道是谁在申请。
		if record["fromNickname"] == request.FromUserID {
			if clawUser, err := h.queries.GetClawIMUserByUserID(ctx, request.FromUserID); err == nil {
				if name := stringOrNull(clawUser.Nickname); name != "" {
					record["fromNickname"] = name
				} else if strings.TrimSpace(clawUser.Username) != "" {
					record["fromNickname"] = clawUser.Username
				}
				if portrait := stringOrNull(clawUser.PortraitUri); portrait != "" {
					record["fromPortraitUri"] = portrait
				}
			}
		}
	}
	return record
}

// nodeDisplayName 取节点的展示名（rongcloud_user.name，与 ai/getNodes 同源，
// 见 system_handler.go:381-384）。取不到就退回融云 id，前端至少有东西可显示。
func (h *systemHandler) nodeDisplayName(ctx context.Context, nodeRCID string) string {
	if h.queries != nil {
		if user, err := h.queries.GetRongCloudUserByRongCloudID(ctx, nodeRCID); err == nil {
			if user.Name.Valid && strings.TrimSpace(user.Name.String) != "" {
				return user.Name.String
			}
		}
	}
	return nodeRCID
}

// friendRequestRemark 是投递到融云侧那条申请的验证信息。
//
// 融云团队在客户端只透传一个 extra 字符串（SDK 的 addFriend 只有三个参数），
// 结构化字段存不住，所以把设备名拼进备注，让拥有者在「新的朋友」里也能看出
// 「对方要加的是哪台设备」——与移动端 buildDeviceApplicationMessage 保持一致。
func (h *systemHandler) friendRequestRemark(ctx context.Context, message, nodeRCID string) string {
	name := h.nodeDisplayName(ctx, nodeRCID)
	base := strings.TrimSpace(message)
	if name == "" || name == nodeRCID {
		return base
	}
	if base == "" {
		return "申请使用设备：" + name
	}
	return base + "｜申请使用设备：" + name
}

// ensureVisibleAgentNode 确认这个融云 id 对应一台真实、可对外展示的 agent 节点。
// 机器主节点与 ops 运维节点是内部节点（isHiddenNode，node_service.go:254），
// 不能被当成「智能体」加好友。
func (h *systemHandler) ensureVisibleAgentNode(ctx context.Context, nodeRCID string) error {
	if h.queries == nil {
		// 没有 DB 装配（单测、未接库的部署）时无法校验节点行；审批表本身也
		// 装配不出来，这条路径在生产不可达。这里放行，由 NodeOwner 做最终判定。
		return nil
	}
	node, err := h.queries.GetRongCloudNodeByRongCloudUserID(ctx, nodeRCID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errFriendNodeNotFound
		}
		return err
	}
	if isHiddenNode(node) {
		return errFriendNodeHidden
	}
	return nil
}

var (
	errFriendNodeNotFound = errors.New("rongcloud: agent node not found")
	errFriendNodeHidden   = errors.New("rongcloud: agent node is internal")
)

func (h *systemHandler) replyErrorForNode(ctx context.Context, msg NormalizedMessage, cmd CommandContent, err error) error {
	switch {
	case errors.Is(err, errFriendNodeNotFound):
		return h.replyErr(ctx, msg, cmd, 404, "agent node not found")
	case errors.Is(err, errFriendNodeHidden):
		return h.replyErr(ctx, msg, cmd, 400, "internal nodes cannot be added as friends")
	default:
		h.logger.Error("rongcloud: friend request node lookup", "error", err, "node", cmd.Params["deviceNodeId"])
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the agent node")
	}
}
