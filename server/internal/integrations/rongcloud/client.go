package rongcloud

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultAPIBase = "https://api.rong-api.com"

// rongcloudAPIClient sends messages via RongCloud Server REST API.
type rongcloudAPIClient struct {
	appKey    string
	appSecret string
	base      string
	client    *http.Client
	logger    *slog.Logger
	// loadCreds, when set, resolves credentials from the active channel
	// installation. Resolution happens at most once per process: on success
	// the values cache for the client's lifetime; rotation requires a
	// process restart. Guarded by credsMu.
	loadCreds func(ctx context.Context) (appKey, appSecret string, err error)
	credsMu   sync.Mutex
	// credsResolved is set once credentials are known (either supplied at
	// construction or successfully loaded), so failed loads can retry
	// without re-running after a success.
	credsResolved bool
	// presence 是「节点融云 id → 是否在线」的短 TTL 缓存，只被 nodeIsOnline
	// 使用（见 node_presence_cache.go）。放在 client 上而不是包级变量，是为了让
	// 它天然跟着「一个融云凭据上下文」走：用例里每个假服务各建一个 client，
	// 互不串味；生产里一个进程一个 client，正好只合并同一进程内的重复探测。
	// nil 表示不缓存（只有用例才会这样构造）。
	presence *nodePresenceCache
}

func newRongCloudAPIClient(appKey, appSecret, base string, client *http.Client, logger *slog.Logger) *rongcloudAPIClient {
	if base == "" {
		base = defaultAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &rongcloudAPIClient{
		appKey:    appKey,
		appSecret: appSecret,
		base:      base,
		client:    client,
		logger:    logger,
		presence:  newNodePresenceCache(nodePresenceCacheTTL),
	}
}

const nonceChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateNonce produces a random alphanumeric string of the given length.
func generateNonce(length int) string {
	b := make([]byte, length)
	max := big.NewInt(int64(len(nonceChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			b[i] = nonceChars[0]
			continue
		}
		b[i] = nonceChars[n.Int64()]
	}
	return string(b)
}

// computeSignature computes SHA1(AppSecret + Nonce + Timestamp) as lowercase hex.
func computeSignature(appSecret, nonce string, timestamp int64) string {
	return computeSignatureFromString(appSecret, nonce, fmt.Sprintf("%d", timestamp))
}

// computeSignatureFromString computes SHA1(AppSecret + Nonce + Timestamp) from string timestamp.
func computeSignatureFromString(appSecret, nonce, timestamp string) string {
	source := appSecret + nonce + timestamp
	h := sha1.Sum([]byte(source))
	return hex.EncodeToString(h[:])
}

func (c *rongcloudAPIClient) signRequest(req *http.Request, contentType string) {
	nonce := generateNonce(18)
	timestamp := time.Now().UnixMilli()
	signature := computeSignature(c.appSecret, nonce, timestamp)
	req.Header.Set("App-Key", c.appKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Nonce", nonce)
	req.Header.Set("Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Signature", signature)
}

// ensureCreds populates appKey/appSecret for service-layer clients that were
// constructed without credentials, resolving them from the active channel
// installation exactly once: concurrent callers wait on the mutex while the
// first resolves; a failed resolution leaves the client unresolved so the
// next request retries, while a success caches for the process lifetime.
func (c *rongcloudAPIClient) ensureCreds(ctx context.Context) error {
	if c.loadCreds == nil {
		return nil
	}
	c.credsMu.Lock()
	defer c.credsMu.Unlock()
	if c.credsResolved {
		return nil
	}
	appKey, appSecret, err := c.loadCreds(ctx)
	if err != nil {
		return fmt.Errorf("rongcloud: load installation credentials: %w", err)
	}
	if appKey == "" || appSecret == "" {
		return fmt.Errorf("rongcloud: no active installation with app credentials")
	}
	c.appKey, c.appSecret = appKey, appSecret
	c.credsResolved = true
	return nil
}

// postForm sends a POST request with form-urlencoded body and returns the parsed JSON response.
func (c *rongcloudAPIClient) postForm(ctx context.Context, path string, form url.Values) (map[string]interface{}, error) {
	if err := c.ensureCreds(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rongcloud: create request: %w", err)
	}
	c.signRequest(req, "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: send request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: read response: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("rongcloud: parse response: %w", err)
	}
	if code, ok := result["code"].(float64); ok && code != 200 {
		message, _ := result["errorMessage"].(string)
		return result, &APIError{Code: int(code), Message: message}
	}
	return result, nil
}

// APIError 是融云的业务错误（响应体 `code != 200`）。
//
// 之前这里只返回 `fmt.Errorf`，调用方想区分「用户不存在」「不是好友」这类
// 具体错误码就只能比对错误字符串 —— 那既脆又易错。现在有了类型，用
// `errors.As` 取码即可；`Error()` 的输出和旧格式逐字一致，靠字符串匹配的
// 老调用方（如 friend_request.go 的 25460 判定）不受影响。
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("rongcloud: API error code %d: %s", e.Code, e.Message)
}

// RongCloudAPIError 把 err 还原成 *APIError；不是融云业务错误时 ok=false。
func RongCloudAPIError(err error) (code int, message string, ok bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return 0, "", false
	}
	return apiErr.Code, apiErr.Message, true
}

// sendPrivateMessage sends a message to a specific user via RongCloud.
func (c *rongcloudAPIClient) sendPrivateMessage(ctx context.Context, fromUserID, toUserID, objectName, content string) (string, error) {
	form := url.Values{
		"fromUserId":  {fromUserID},
		"toUserId":    {toUserID},
		"objectName":  {objectName},
		"content":     {content},
		"isPersisted": {"1"},
		"isCounted":   {"1"},
	}
	result, err := c.postForm(ctx, "/message/private/publish.json", form)
	if err != nil {
		return "", err
	}
	if msgUID, ok := result["msgUID"].(string); ok {
		return msgUID, nil
	}
	return "", nil
}

// sendChatroomMessage publishes a message into a RongCloud chatroom.
//
// P7 uses it for the v3 discovery events: the coordinator writes one row per event
// and then fans the same payload out to the room so the C-end timeline updates
// without polling. The legacy Python transport did exactly this with
// from_user_id="system" / object_name="command" and no persistence flags, and the
// device-side protocol never relied on these being durable — the web re-syncs
// over HTTP when it opens a room — so we keep the same shape.
func (c *rongcloudAPIClient) sendChatroomMessage(ctx context.Context, fromUserID, chatroomID, objectName, content string) (string, error) {
	form := url.Values{
		"fromUserId":   {fromUserID},
		"toChatroomId": {chatroomID},
		"objectName":   {objectName},
		"content":      {content},
	}
	result, err := c.postForm(ctx, "/message/chatroom/publish.json", form)
	if err != nil {
		return "", err
	}
	if msgUID, ok := result["msgUID"].(string); ok {
		return msgUID, nil
	}
	return "", nil
}

// sendGroupMessage publishes a message into a RongCloud group (融云普通群组，
// conversationType=3) as a specific member. Used by the group relay to speak AI
// members' answers back into the group as normal group bubbles. The legacy
// Python transport used the same endpoint with isPersisted=0/isCounted=0;
// groups here are durable conversations, so we persist and count.
func (c *rongcloudAPIClient) sendGroupMessage(ctx context.Context, fromUserID, groupID, objectName, content string) (string, error) {
	form := url.Values{
		"fromUserId": {fromUserID},
		"toGroupId":  {groupID},
		"objectName": {objectName},
		"content":    {content},
	}
	result, err := c.postForm(ctx, "/message/group/publish.json", form)
	if err != nil {
		return "", err
	}
	if msgUID, ok := result["msgUID"].(string); ok {
		return msgUID, nil
	}
	return "", nil
}

// sendCommandResult sends a command_result message back to the requesting user.
func (c *rongcloudAPIClient) sendCommandResult(ctx context.Context, fromUserID, toUserID, requestID string, payload map[string]interface{}) error {
	resultContent := CommandResultContent{
		RequestID: requestID,
		MsgType:   "command_result",
		Payload:   payload,
	}
	contentBytes, err := json.Marshal(resultContent)
	if err != nil {
		return fmt.Errorf("rongcloud: marshal command result: %w", err)
	}
	_, err = c.sendPrivateMessage(ctx, fromUserID, toUserID, objectNameCommand, string(contentBytes))
	return err
}

// sendWebCommandResult answers the web client's system-command channel.
//
// The web's handleSystemResponse (clawmessenger-web/src/services/im.ts) walks the
// message content looking for an object that carries requestId-or-request_id
// AND a string `status`, then reads `code` and `data` off it. That is a FLAT
// object, not the device-shaped {request_id, msg_type, payload} envelope the CLI
// parses, so the two reply paths cannot share an encoder. msg_type
// "command_result" is still set: it is what keeps the reply from being
// re-interpreted as a fresh request when 融云 echoes it back to the webhook.
func (c *rongcloudAPIClient) sendWebCommandResult(ctx context.Context, fromUserID, toUserID, requestID, service, action, status string, code int, data interface{}, message string) error {
	payload := map[string]interface{}{
		"requestId": requestID,
		"service":   service,
		"action":    action,
		"status":    status,
		"code":      code,
		"message":   message,
		"msg_type":  "command_result",
		"timestamp": time.Now().UnixMilli(),
	}
	if data != nil {
		payload["data"] = data
	}
	contentBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("rongcloud: marshal web command result: %w", err)
	}
	_, err = c.sendPrivateMessage(ctx, fromUserID, toUserID, objectNameCommand, string(contentBytes))
	return err
}
