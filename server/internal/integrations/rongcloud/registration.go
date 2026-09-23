package rongcloud

import (
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

type ChannelDeps struct {
	Registrar  WebhookRegistrar
	Decrypt    Decrypter
	Logger     *slog.Logger
	APIBase    string
	HTTPClient *http.Client
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
		return &rongcloudChannel{
			cfg:           cfg,
			creds:         creds,
			client:        client,
			systemHandler: newSystemHandler(client, creds.SystemNodeID, logger),
			registrar:     deps.Registrar,
			handler:       cfg.Handler,
			logger:        logger,
		}, nil
	}
}
