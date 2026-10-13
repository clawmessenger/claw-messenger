package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
	// commands 用于丢弃融云重投的命令回调（见 command_dispatch.go）。
	commands commandDedup
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
	// P7：交互轮次引擎的恢复扫描。派发命令的 available_at 就是该轮的等待上限，
	// 到点仍未被设备回包的命令按超时处理（重试 / 跳过 / 判定失败）。它是持久
	// outbox 上的扫描，因此进程重启后也能接着推进，不需要内存态。
	if rt := c.systemHandler.discussionRuntime; rt != nil {
		go c.runInteractiveSweep(ctx, rt)
	}
	<-ctx.Done()
	c.registrar.Unregister(installationID)
	c.logger.Info("rongcloud channel disconnected", "installation_id", installationID)
	return nil
}

// runInteractiveSweep 周期驱动 v3 轮次引擎的超时恢复，直到 channel 上下文结束。
func (c *rongcloudChannel) runInteractiveSweep(ctx context.Context, rt *discussionRuntime) {
	ticker := time.NewTicker(interactiveSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.interactiveSweep(ctx)
		}
	}
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
			// 设备回包只做内存里的一次转交（唤醒 broker / 投给讨论协调器），
			// 必须同步且立即，别拖慢等待方。
			c.systemHandler.handleNodeMessage(r.Context(), msg)
			w.WriteHeader(http.StatusOK)
			return
		}
		// 命令一律先 200 再处理：recommendRoles / createRoundtable 要走融云
		// P2P 下发给设备、等设备端 agent 跑完才回包，同步做会让融云的回调
		// 客户端先超时断开（r.Context() 被取消，回包发不出去），还会触发重投
		// 让设备重复执行。详见 command_dispatch.go。
		c.dispatchCommandAsync(r.Context(), msg)
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
