package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

type rongcloudChannel struct {
	cfg           channel.Config
	creds         credentials
	client        *rongcloudAPIClient
	systemHandler *systemHandler
	registrar     WebhookRegistrar
	handler       channel.InboundHandler
	logger        *slog.Logger
	ctx           context.Context
	cancel        context.CancelFunc
}

func (c *rongcloudChannel) Type() channel.Type {
	return TypeRongCloud
}

func (c *rongcloudChannel) Capabilities() channel.Capability {
	return channel.CapText
}

func (c *rongcloudChannel) Connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.ctx = ctx
	c.cancel = cancel

	installationID := c.installationID()
	if err := c.registrar.Register(installationID, c.handleWebhook); err != nil {
		return fmt.Errorf("rongcloud: register webhook: %w", err)
	}

	c.logger.Info("rongcloud channel connected", "installation_id", installationID)
	<-ctx.Done()
	c.registrar.Unregister(installationID)
	c.logger.Info("rongcloud channel disconnected", "installation_id", installationID)
	return nil
}

func (c *rongcloudChannel) Disconnect(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	installationID := c.installationID()
	c.registrar.Unregister(installationID)
	c.logger.Info("rongcloud channel disconnected", "installation_id", installationID)
	return nil
}

func (c *rongcloudChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	content := encodeTextContent(out.Text)
	msgUID, err := c.client.sendPrivateMessage(ctx, c.creds.SystemNodeID, out.ChatID, objectNameText, content)
	if err != nil {
		return channel.SendResult{}, fmt.Errorf("rongcloud: send: %w", err)
	}
	return channel.SendResult{MessageID: msgUID}, nil
}

func (c *rongcloudChannel) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if !verifyWebhookSignature(c.creds.AppSecret, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	msg, err := parseWebhookPayload(r)
	if err != nil {
		c.logger.Error("rongcloud: parse webhook payload", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	msgID := msg.MsgUID
	if msgID == "" {
		msgID = fmt.Sprintf("%s_%s_%s", msg.FromUserID, msg.ToUserID, msg.MsgTimeStamp)
	}

	if isCommandMessage(msg.ObjectName) {
		if isCommandResult(msg) {
			c.systemHandler.handleNodeMessage(r.Context(), msg)
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := c.systemHandler.handleCommand(r.Context(), msg); err != nil {
			c.logger.Error("rongcloud: handle command", "error", err, "msgUID", msgID)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if isStreamMessage(msg.ObjectName) {
		c.systemHandler.handleNodeMessage(r.Context(), msg)
		w.WriteHeader(http.StatusOK)
		return
	}

	inbound, ok := normalizeInbound(msg)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := c.handler(r.Context(), inbound); err != nil {
		c.logger.Error("rongcloud: inbound handler", "error", err, "msgUID", msgID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *rongcloudChannel) installationID() string {
	if c.cfg.ID.Valid {
		return c.cfg.ID.String()
	}
	return "default"
}
