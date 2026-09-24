package rongcloud

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// WebhookRegistrar allows dynamic registration and unregistration of
// webhook handlers keyed by installation ID.
type WebhookRegistrar interface {
	Register(installationID string, handler http.HandlerFunc) error
	Unregister(installationID string)
}

// webhookDispatcher implements both WebhookRegistrar and http.Handler.
// It routes incoming webhooks to the correct channel instance by installation ID
// (passed as the "inst" query parameter).
type webhookDispatcher struct {
	mu       sync.RWMutex
	handlers map[string]http.HandlerFunc
	logger   *slog.Logger
}

// NewWebhookDispatcher creates a dispatcher that implements both WebhookRegistrar
// and http.Handler. Register it as a Chi route: r.Post("/api/webhooks/rongcloud", dispatcher).
func NewWebhookDispatcher(logger *slog.Logger) *webhookDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &webhookDispatcher{
		handlers: make(map[string]http.HandlerFunc),
		logger:   logger,
	}
}

func (d *webhookDispatcher) Register(installationID string, handler http.HandlerFunc) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[installationID] = handler
	return nil
}

func (d *webhookDispatcher) Unregister(installationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.handlers, installationID)
}

func (d *webhookDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	inst := r.URL.Query().Get("inst")
	if inst == "" {
		http.Error(w, "missing inst parameter", http.StatusBadRequest)
		return
	}
	d.mu.RLock()
	handler, ok := d.handlers[inst]
	d.mu.RUnlock()
	if !ok {
		http.Error(w, "installation not found", http.StatusNotFound)
		return
	}
	handler(w, r)
}

// getWebhookParam looks up a value from request headers (case-insensitive) then
// query parameters, trying each name in order. RongCloud sends signature fields
// as either "rc-nonce"/"nonce" headers or query params.
func getWebhookParam(r *http.Request, names ...string) string {
	for _, name := range names {
		if v := r.Header.Get(name); v != "" {
			return v
		}
	}
	q := r.URL.Query()
	for _, name := range names {
		if v := q.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// verifyWebhookSignature validates the RongCloud webhook signature.
// Signature = SHA1(AppSecret + Nonce + Timestamp), case-insensitive comparison.
// The body is NOT part of the signature.
func verifyWebhookSignature(appSecret string, r *http.Request) bool {
	nonce := getWebhookParam(r, "rc-nonce", "nonce")
	timestamp := getWebhookParam(r, "rc-timestamp", "timestamp")
	signature := getWebhookParam(r, "rc-signature", "signature")
	if nonce == "" || timestamp == "" || signature == "" {
		return false
	}
	expected := computeSignatureFromString(appSecret, nonce, timestamp)
	return hmac.Equal([]byte(strings.ToLower(expected)), []byte(strings.ToLower(signature)))
}

// parseWebhookPayload extracts the RongCloud webhook payload from either JSON or
// form-urlencoded request body.
func parseWebhookPayload(r *http.Request) (NormalizedMessage, error) {
	var raw map[string]interface{}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return NormalizedMessage{}, fmt.Errorf("rongcloud: read webhook body: %w", err)
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return NormalizedMessage{}, fmt.Errorf("rongcloud: parse webhook JSON: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return NormalizedMessage{}, fmt.Errorf("rongcloud: parse webhook form: %w", err)
		}
		raw = make(map[string]interface{})
		for k, v := range r.Form {
			if len(v) > 0 {
				raw[k] = v[0]
			}
		}
	}
	return NormalizedMessage{
		ObjectName:       getString(raw, "objectName"),
		FromUserID:       getString(raw, "fromUserId"),
		ToUserID:         getString(raw, "toUserId"),
		TargetID:         getString(raw, "targetId"),
		Content:          getString(raw, "content"),
		MsgUID:           getString(raw, "msgUID"),
		MsgTimeStamp:     getString(raw, "msgTimeStamp"),
		ConversationType: getString(raw, "conversationType"),
	}, nil
}
