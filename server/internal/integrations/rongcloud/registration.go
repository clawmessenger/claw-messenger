package rongcloud

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/quota"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ChannelDeps struct {
	Registrar  WebhookRegistrar
	Decrypt    Decrypter
	Logger     *slog.Logger
	APIBase    string
	HTTPClient *http.Client
	Queries    *db.Queries
	Registry   *DiscussionRegistry
	// NodeRequests brokers server-initiated requests answered by a device over
	// the command channel (currently the admin role-recommendation preview). The
	// same instance is held by the HTTP layer, which starts requests from the
	// admin API; the channel binds the sender and routes the answers in. Nil
	// disables the feature.
	NodeRequests *NodeRequestBroker
	// GroupMirror supplies the database side of the `service=group` commands
	// (the RongCloud group APIs live in this package, the tables in the HTTP
	// layer). Nil disables the group commands.
	GroupMirror *GroupMirror
	// DiscussionRepository is the roundtable persistence layer (P2). The
	// service=discussion lifecycle actions (P4) drive it. Nil disables those
	// actions.
	DiscussionRepository *DiscussionRepository
	// SystemHostConfig loads the admin-configured discussion system host
	// (display name / avatar / node). Nil falls back to NOT_CONFIGURED.
	SystemHostConfig func(ctx context.Context) (DiscussionSystemHostConfig, bool, error)
	// Quota 是讨论组/群/聊天室的创建配额服务（T01 internal/quota）。挂在
	// discussionRuntime 上供 createRoundtableShell 硬校验；Nil 表示不启用
	// 配额（全部放行，不引入新的失败面）。
	Quota *quota.Service
}

func RegisterRongCloud(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeRongCloud, newRongCloudFactory(deps))
}

func newRongCloudFactory(deps ChannelDeps) channel.Factory {
	return func(cfg channel.Config) (channel.Channel, error) {
		creds, err := decodeCredentials(cfg.Raw, deps.Decrypt)
		if err != nil {
			return nil, err
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		client := newRongCloudAPIClient(creds.AppKey, creds.AppSecret, deps.APIBase, deps.HTTPClient, logger)
		// The broker needs the credential-resolved system node id to send, which
		// only exists here — hence the bind rather than a constructor argument.
		if deps.NodeRequests != nil {
			systemNodeID := creds.SystemNodeID
			deps.NodeRequests.BindSender(func(ctx context.Context, toUserID, content string) error {
				_, err := client.sendPrivateMessage(ctx, systemNodeID, toUserID, objectNameCommand, content)
				return err
			})
		}
		var discussionRuntime *discussionRuntime
		if deps.DiscussionRepository != nil {
			discussionRuntime = newDiscussionRuntime(deps.Queries, deps.DiscussionRepository, client, creds.SystemNodeID, logger)
			discussionRuntime.systemHostLoader = deps.SystemHostConfig
			// P5：创建编排与 recommendRoles 的 broker 都挂在 runtime 上。
			// nodeOwner 按「融云好友边」列用户归属节点（node_claim 语义的反向）：
			// 好友列表里的 rc 账号经 GetRongCloudNodeByRongCloudUserID 映射回
			// 节点行。机器主节点 ai_type 为空 → 无讨论能力位，天然不进 eligible。
			creation := newRoundtableCreationService(deps.Queries, client)
			creation.hostLoader = deps.SystemHostConfig
			creation.nodeOwner = friendEdgeNodeOwner(deps.Queries, client, logger)
			discussionRuntime.attachRoundtable(creation, deps.NodeRequests)
			// P7：v3 交互轮次引擎就绪。interactiveEnabled 是 getWorkflowCapabilities
			// 翻到 [2,3] 的唯一开关；workflowAvailable 让创建编排用同一个开关判定
			// user_confirmed 是否可跑 —— 二者必须同源，否则会出现「能力已宣告但
			// 创建被拒」或反过来「创建放行但前端不敢用」的错位。
			discussionRuntime.interactiveEnabled = true
			creation.workflowAvailable = discussionRuntime.roundtablePolicyAvailable
			// 配额服务（Nil 放行）：空组直建 / 群创建 / 聊天室创建三入口共用。
			discussionRuntime.quota = deps.Quota
		}
		// 讨论组闲聊与普通群组的「按序发言」中继（服务端仲裁，替代客户端扇出）。
		// groupMirror 由 HTTP 层注入，群中继用它读成员镜像。
		var relayHub *chatroomRelayHub
		if discussionRuntime != nil {
			relayHub = newChatroomRelayHub(discussionRuntime)
			relayHub.groupMirror = deps.GroupMirror
		}
		return &rongcloudChannel{
			cfg:           cfg,
			creds:         creds,
			client:        client,
			systemHandler: newSystemHandler(client, creds.SystemNodeID, logger, deps.Queries, deps.Registry, deps.NodeRequests, deps.GroupMirror, discussionRuntime, relayHub),
			registrar:     deps.Registrar,
			handler:       cfg.Handler,
			logger:        logger,
		}, nil
	}
}
