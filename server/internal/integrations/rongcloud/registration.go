package rongcloud

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
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
		return &rongcloudChannel{
			cfg:           cfg,
			creds:         creds,
			client:        client,
			systemHandler: newSystemHandler(client, creds.SystemNodeID, logger, deps.Queries, deps.Registry, deps.NodeRequests, deps.GroupMirror),
			registrar:     deps.Registrar,
			handler:       cfg.Handler,
			logger:        logger,
		}, nil
	}
}
