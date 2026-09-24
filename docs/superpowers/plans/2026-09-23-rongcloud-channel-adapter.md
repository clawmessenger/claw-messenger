# RongCloud Channel Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Build a RongCloud (èžäº‘) Channel adapter that receives messages via webhook and sends via REST API, with a commandâ†’command_result RPC loop (ping) as the MVP business logic.

**Architecture:** The adapter lives in `server/internal/integrations/rongcloud/`, registered to the existing `channel.Registry` â€?zero core code changes except router wiring. It is the first webhook-based IM adapter (all others use persistent connections). A `WebhookDispatcher` (implementing both `WebhookRegistrar` and `http.Handler`) routes incoming webhooks to the correct channel instance by installation ID. The adapter intercepts command messages for internal system-node processing and passes non-command messages to the engine-injected `InboundHandler`.

**Tech Stack:** Go, Chi router, `net/http`, `crypto/sha1`, `encoding/json`, `slog`, `httptest` for tests. No external Go dependencies â€?only stdlib + the existing multica packages.

**Spec:** `docs/superpowers/specs/2026-09-23-rongcloud-channel-adapter-design.md`

## Global Constraints

- Module path: `github.com/multica-ai/multica`
- Channel package: `github.com/multica-ai/multica/server/internal/integrations/channel`
- Code comments in English; atomic conventional commits
- No foreign keys/cascades; CONCURRENTLY indexes (not relevant for MVP, no DB)
- No compatibility shims/dual writes/legacy adapters
- Tests live beside implementation; use `httptest.NewServer` for mock API
- `make test` runs Go backend tests
- RongCloud signature: `SHA1(AppSecret + Nonce + Timestamp)` â€?no HMAC, plain string concatenation
- RongCloud objectName for command messages is the literal string `"command"` (not `RC:CmdMsg`)
- Command results are sent with objectName `"command"` and content containing `msg_type: "command_result"`

---

## File Structure

All new files in `server/internal/integrations/rongcloud/`:

| File | Responsibility |
|---|---|
| `types.go` | Constants (`TypeRongCloud`, objectName strings), `NormalizedMessage`, `CommandContent`, `CommandResultContent`, `getString` helper |
| `config.go` | `installConfig`, `credentials`, `Decrypter` type, `decodeCredentials`, `decryptSecret` |
| `client.go` | `rongcloudAPIClient`: `generateNonce`, `computeSignature`, `computeSignatureFromString`, `signRequest`, `postForm`, `sendPrivateMessage`, `sendCommandResult` |
| `webhook.go` | `WebhookRegistrar` interface, `webhookDispatcher` (+ `NewWebhookDispatcher`), `verifyWebhookSignature`, `getWebhookParam`, `parseWebhookPayload` |
| `inbound.go` | `normalizeInbound`: converts `NormalizedMessage` â†?`channel.InboundMessage`; `textContent` struct |
| `outbound.go` | `encodeTextContent`: wraps plain text into RongCloud `{"content":"..."}` JSON |
| `system_handler.go` | `systemHandler`: `handleCommand` parses command, dispatches by service; `handlePing` echo |
| `channel.go` | `rongcloudChannel`: implements `channel.Channel` (`Type`, `Capabilities`, `Connect`, `Disconnect`, `Send`); `handleWebhook` method |
| `registration.go` | `ChannelDeps`, `RegisterRongCloud`, `newRongCloudFactory` |
| `rongcloud_test.go` | All unit tests |

Modified file:

| File | Change |
|---|---|
| `server/cmd/server/router.go` | Add import, env-gated RongCloud registration block (~after Telegram block line ~1206), add webhook route `r.Post("/api/webhooks/rongcloud", ...)` near line ~1491 |

---

## Task 1: types.go â€?Constants and Core Structs

**Files:**
- Create: `server/internal/integrations/rongcloud/types.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Produces: `TypeRongCloud`, objectName constants, `NormalizedMessage`, `CommandContent`, `CommandResultContent`, `getString`

- [x] **Step 1: Write the failing test**

Create `rongcloud_test.go`:

```go
package rongcloud

import (
	"encoding/json"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCommandContentUnmarshal(t *testing.T) {
	raw := `{"request_id":"req_123","service":"ping","action":"ping","params":{"echo":"hello"}}`
	var cmd CommandContent
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmd.RequestID != "req_123" {
		t.Errorf("RequestID = %q, want %q", cmd.RequestID, "req_123")
	}
	if cmd.Service != "ping" {
		t.Errorf("Service = %q, want %q", cmd.Service, "ping")
	}
	if cmd.Action != "ping" {
		t.Errorf("Action = %q, want %q", cmd.Action, "ping")
	}
	if cmd.Params["echo"] != "hello" {
		t.Errorf("Params[echo] = %v, want hello", cmd.Params["echo"])
	}
}

func TestCommandResultContentMarshal(t *testing.T) {
	result := CommandResultContent{
		RequestID: "req_123",
		MsgType:   "command_result",
		Payload:   map[string]interface{}{"ok": true, "message": "pong"},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if m["request_id"] != "req_123" {
		t.Errorf("request_id = %v, want req_123", m["request_id"])
	}
	if m["msg_type"] != "command_result" {
		t.Errorf("msg_type = %v, want command_result", m["msg_type"])
	}
}

func TestTypeRongCloudConstant(t *testing.T) {
	if TypeRongCloud != "rongcloud" {
		t.Errorf("TypeRongCloud = %q, want %q", TypeRongCloud, "rongcloud")
	}
}
```

Also add these imports at the top of the test file (they will be used by later tasks too):

```go
import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)
```

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestCommandContentUnmarshal -v`
Expected: FAIL â€?`TypeRongCloud` undefined, `CommandContent` undefined, `CommandResultContent` undefined

- [x] **Step 3: Write minimal implementation**

Create `types.go`:

```go
package rongcloud

import "github.com/multica-ai/multica/server/internal/integrations/channel"

// TypeRongCloud is the channel type slug for RongCloud installations.
const TypeRongCloud channel.Type = "rongcloud"

// RongCloud message objectName constants.
const (
	objectNameText    = "RC:TxtMsg"
	objectNameImage   = "RC:ImgMsg"
	objectNameCommand = "command"
	objectNameStream  = "RC:StreamMsg"
	objectNameTyping  = "RC:TypSts"
)

// NormalizedMessage represents a parsed RongCloud webhook payload.
type NormalizedMessage struct {
	ObjectName       string `json:"objectName"`
	FromUserID       string `json:"fromUserId"`
	ToUserID         string `json:"toUserId"`
	TargetID         string `json:"targetId"`
	Content          string `json:"content"`
	MsgUID           string `json:"msgUID"`
	MsgTimeStamp     string `json:"msgTimeStamp"`
	ConversationType string `json:"conversationType"`
}

// CommandContent is the parsed JSON content of a command message.
type CommandContent struct {
	RequestID string                 `json:"request_id"`
	Service   string                 `json:"service"`
	Action    string                 `json:"action"`
	Params    map[string]interface{} `json:"params,omitempty"`
}

// CommandResultContent is the JSON content sent back as a command_result.
type CommandResultContent struct {
	RequestID string                 `json:"request_id"`
	MsgType   string                 `json:"msg_type"`
	Payload   map[string]interface{} `json:"payload"`
}

// getString extracts a string value from a map, returning "" if missing or not a string.
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestCommandContentUnmarshal -v`
Expected: PASS

Also run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestCommandResultContentMarshal|TestTypeRongCloudConstant" -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/types.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add type constants and core message structs"
```

---

## Task 2: config.go â€?Installation Config and Credentials

**Files:**
- Create: `server/internal/integrations/rongcloud/config.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `getString` from Task 1 (not needed here, config uses raw JSON)
- Produces: `installConfig`, `credentials`, `Decrypter`, `decodeCredentials`, `decryptSecret`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestDecodeCredentialsValid(t *testing.T) {
	// Encrypt "my-secret" with a no-op decrypter (identity)
	secretB64 := base64.StdEncoding.EncodeToString([]byte("my-secret"))
	raw := json.RawMessage(`{"app_key":"app123","app_secret_encrypted":"` + secretB64 + `","system_node_id":"sys_node_1"}`)
	creds, err := decodeCredentials(raw, func(ciphertext []byte) ([]byte, error) {
		return ciphertext, nil // identity decrypter
	})
	if err != nil {
		t.Fatalf("decodeCredentials: %v", err)
	}
	if creds.AppKey != "app123" {
		t.Errorf("AppKey = %q, want app123", creds.AppKey)
	}
	if creds.AppSecret != "my-secret" {
		t.Errorf("AppSecret = %q, want my-secret", creds.AppSecret)
	}
	if creds.SystemNodeID != "sys_node_1" {
		t.Errorf("SystemNodeID = %q, want sys_node_1", creds.SystemNodeID)
	}
}

func TestDecodeCredentialsMissingAppKey(t *testing.T) {
	raw := json.RawMessage(`{"app_secret_encrypted":"dGVzdA==","system_node_id":"sys"}`)
	_, err := decodeCredentials(raw, func(b []byte) ([]byte, error) { return b, nil })
	if err == nil {
		t.Fatal("expected error for missing app_key")
	}
}

func TestDecodeCredentialsDecryptError(t *testing.T) {
	raw := json.RawMessage(`{"app_key":"k","app_secret_encrypted":"dGVzdA==","system_node_id":"s"}`)
	_, err := decodeCredentials(raw, func(b []byte) ([]byte, error) {
		return nil, errors.New("decrypt failed")
	})
	if err == nil {
		t.Fatal("expected error for decrypt failure")
	}
}
```

Add `"encoding/base64"` and `"errors"` to the test file imports.

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestDecodeCredentials -v`
Expected: FAIL â€?`decodeCredentials` undefined

- [x] **Step 3: Write minimal implementation**

Create `config.go`:

```go
package rongcloud

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// installConfig is the JSON shape stored in channel_installation.config JSONB.
type installConfig struct {
	AppKey             string `json:"app_key"`
	AppSecretEncrypted string `json:"app_secret_encrypted"`
	SystemNodeID       string `json:"system_node_id"`
}

// credentials holds the decrypted RongCloud API credentials.
type credentials struct {
	AppKey       string
	AppSecret    string
	SystemNodeID string
}

// Decrypter decrypts ciphertext produced by the secretbox encryption layer.
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials unmarshals the installation config and decrypts the app secret.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	var ic installConfig
	if err := json.Unmarshal(raw, &ic); err != nil {
		return credentials{}, fmt.Errorf("rongcloud: unmarshal install config: %w", err)
	}
	if strings.TrimSpace(ic.AppKey) == "" {
		return credentials{}, errors.New("rongcloud: app_key is required")
	}
	if strings.TrimSpace(ic.AppSecretEncrypted) == "" {
		return credentials{}, errors.New("rongcloud: app_secret_encrypted is required")
	}
	if strings.TrimSpace(ic.SystemNodeID) == "" {
		return credentials{}, errors.New("rongcloud: system_node_id is required")
	}
	secret, err := decryptSecret(ic.AppSecretEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("rongcloud: decrypt app secret: %w", err)
	}
	return credentials{
		AppKey:       strings.TrimSpace(ic.AppKey),
		AppSecret:    secret,
		SystemNodeID: strings.TrimSpace(ic.SystemNodeID),
	}, nil
}

// decryptSecret base64-decodes then decrypts the app secret.
func decryptSecret(enc string, decrypt Decrypter) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("rongcloud: base64 decode app secret: %w", err)
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", fmt.Errorf("rongcloud: decrypt: %w", err)
	}
	return strings.TrimSpace(string(plaintext)), nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestDecodeCredentials -v`
Expected: PASS (all 3 sub-tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/config.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add install config and credentials decoding"
```

---

## Task 3: client.go â€?RongCloud REST API Client

**Files:**
- Create: `server/internal/integrations/rongcloud/client.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `CommandResultContent`, `objectNameCommand` from Task 1
- Produces: `rongcloudAPIClient`, `newRongCloudAPIClient`, `generateNonce`, `computeSignature`, `computeSignatureFromString`, `sendPrivateMessage`, `sendCommandResult`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestComputeSignature(t *testing.T) {
	sig := computeSignature("my-secret", "abc123nonce456", 1695494400000)
	// Verify it's a 40-char lowercase hex SHA1
	if len(sig) != 40 {
		t.Fatalf("signature length = %d, want 40", len(sig))
	}
	// Verify determinism: same inputs produce same output
	sig2 := computeSignature("my-secret", "abc123nonce456", 1695494400000)
	if sig != sig2 {
		t.Fatalf("signature not deterministic: %q vs %q", sig, sig2)
	}
	// Verify it matches manual SHA1(AppSecret + Nonce + Timestamp)
	manual := sha1Hex("my-secret" + "abc123nonce456" + "1695494400000")
	if sig != manual {
		t.Fatalf("signature = %q, manual = %q", sig, manual)
	}
}

func TestGenerateNonce(t *testing.T) {
	n := generateNonce(18)
	if len(n) != 18 {
		t.Fatalf("nonce length = %d, want 18", len(n))
	}
	for _, c := range n {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			t.Fatalf("nonce contains invalid char: %q", c)
		}
	}
}

func TestPostFormSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify signature headers
		if r.Header.Get("App-Key") != "test-key" {
			t.Errorf("App-Key = %q", r.Header.Get("App-Key"))
		}
		if r.Header.Get("Nonce") == "" {
			t.Error("Nonce header missing")
		}
		if r.Header.Get("Signature") == "" {
			t.Error("Signature header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"msgUID":"msg_001"}`)
	}))
	defer srv.Close()

	c := newRongCloudAPIClient("test-key", "test-secret", srv.URL, srv.Client(), testLogger())
	result, err := c.postForm(context.Background(), "/message/private/publish.json", url.Values{
		"fromUserId": {"sys"},
		"toUserId":   {"user1"},
	})
	if err != nil {
		t.Fatalf("postForm: %v", err)
	}
	if result["msgUID"] != "msg_001" {
		t.Errorf("msgUID = %v, want msg_001", result["msgUID"])
	}
}

func TestPostFormAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":401,"errorMessage":"signature failed"}`)
	}))
	defer srv.Close()

	c := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())
	_, err := c.postForm(context.Background(), "/test", url.Values{})
	if err == nil {
		t.Fatal("expected API error")
	}
}

func TestSendCommandResult(t *testing.T) {
	var receivedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	err := c.sendCommandResult(context.Background(), "sys_node", "user1", "req_123", map[string]interface{}{
		"ok":      true,
		"message": "pong",
	})
	if err != nil {
		t.Fatalf("sendCommandResult: %v", err)
	}
	if receivedForm.Get("objectName") != "command" {
		t.Errorf("objectName = %q, want command", receivedForm.Get("objectName"))
	}
	var content CommandResultContent
	if err := json.Unmarshal([]byte(receivedForm.Get("content")), &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if content.RequestID != "req_123" {
		t.Errorf("RequestID = %q", content.RequestID)
	}
	if content.MsgType != "command_result" {
		t.Errorf("MsgType = %q", content.MsgType)
	}
	if content.Payload["message"] != "pong" {
		t.Errorf("Payload[message] = %v", content.Payload["message"])
	}
}
```

Add these imports to the test file:
```go
"context"
"crypto/sha1"
"encoding/hex"
"fmt"
"net/url"
```

Add the `sha1Hex` helper to the test file:
```go
func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestComputeSignature|TestGenerateNonce|TestPostForm|TestSendCommandResult" -v`
Expected: FAIL â€?`computeSignature` undefined, `generateNonce` undefined, `rongcloudAPIClient` undefined

- [x] **Step 3: Write minimal implementation**

Create `client.go`:

```go
package rongcloud

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
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

// postForm sends a POST request with form-urlencoded body and returns the parsed JSON response.
func (c *rongcloudAPIClient) postForm(ctx context.Context, path string, form url.Values) (map[string]interface{}, error) {
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
		return result, fmt.Errorf("rongcloud: API error code %v: %v", code, result["errorMessage"])
	}
	return result, nil
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
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestComputeSignature|TestGenerateNonce|TestPostForm|TestSendCommandResult" -v`
Expected: PASS (all sub-tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/client.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add REST API client with signature and send methods"
```

---

## Task 4: webhook.go â€?Signature Verification, Payload Parsing, and Webhook Dispatcher

**Files:**
- Create: `server/internal/integrations/rongcloud/webhook.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `computeSignatureFromString` from Task 3, `NormalizedMessage`, `getString` from Task 1
- Produces: `WebhookRegistrar` interface, `webhookDispatcher`, `NewWebhookDispatcher`, `verifyWebhookSignature`, `parseWebhookPayload`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestVerifyWebhookSignatureValid(t *testing.T) {
	appSecret := "my-secret"
	nonce := "testnonce123456ab"
	timestamp := "1695494400000"
	sig := computeSignatureFromString(appSecret, nonce, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhook?inst=id1", strings.NewReader(""))
	req.Header.Set("rc-nonce", nonce)
	req.Header.Set("rc-timestamp", timestamp)
	req.Header.Set("rc-signature", sig)

	if !verifyWebhookSignature(appSecret, req) {
		t.Fatal("signature verification failed for valid signature")
	}
}

func TestVerifyWebhookSignatureMissingNonce(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("rc-timestamp", "123")
	req.Header.Set("rc-signature", "abc")
	if verifyWebhookSignature("secret", req) {
		t.Fatal("expected false for missing nonce")
	}
}

func TestVerifyWebhookSignatureQueryParams(t *testing.T) {
	appSecret := "my-secret"
	nonce := "querynonce456789xy"
	timestamp := "1695494400001"
	sig := computeSignatureFromString(appSecret, nonce, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhook?nonce="+nonce+"&timestamp="+timestamp+"&signature="+sig, nil)

	if !verifyWebhookSignature(appSecret, req) {
		t.Fatal("signature verification failed for query param signature")
	}
}

func TestParseWebhookPayloadJSON(t *testing.T) {
	body := `{"objectName":"command","fromUserId":"user1","toUserId":"sys","content":"{\"request_id\":\"r1\",\"service\":\"ping\"}","msgUID":"m1","msgTimeStamp":"123","conversationType":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	msg, err := parseWebhookPayload(req)
	if err != nil {
		t.Fatalf("parseWebhookPayload: %v", err)
	}
	if msg.ObjectName != "command" {
		t.Errorf("ObjectName = %q", msg.ObjectName)
	}
	if msg.FromUserID != "user1" {
		t.Errorf("FromUserID = %q", msg.FromUserID)
	}
	if msg.MsgUID != "m1" {
		t.Errorf("MsgUID = %q", msg.MsgUID)
	}
}

func TestParseWebhookPayloadForm(t *testing.T) {
	form := url.Values{
		"objectName":       {"RC:TxtMsg"},
		"fromUserId":       {"user2"},
		"content":          {`{"content":"hello"}`},
		"msgUID":           {"m2"},
		"conversationType": {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	msg, err := parseWebhookPayload(req)
	if err != nil {
		t.Fatalf("parseWebhookPayload: %v", err)
	}
	if msg.ObjectName != "RC:TxtMsg" {
		t.Errorf("ObjectName = %q", msg.ObjectName)
	}
	if msg.FromUserID != "user2" {
		t.Errorf("FromUserID = %q", msg.FromUserID)
	}
}

func TestWebhookDispatcher(t *testing.T) {
	dispatcher := NewWebhookDispatcher(testLogger())
	called := false
	dispatcher.Register("inst-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook?inst=inst-1", nil)
	w := httptest.NewRecorder()
	dispatcher.ServeHTTP(w, req)

	if !called {
		t.Fatal("handler not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Unregister
	dispatcher.Unregister("inst-1")
	req2 := httptest.NewRequest(http.MethodPost, "/webhook?inst=inst-1", nil)
	w2 := httptest.NewRecorder()
	dispatcher.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("after unregister status = %d, want %d", w2.Code, http.StatusNotFound)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestVerifyWebhookSignature|TestParseWebhookPayload|TestWebhookDispatcher" -v`
Expected: FAIL â€?`verifyWebhookSignature`, `parseWebhookPayload`, `NewWebhookDispatcher` undefined

- [x] **Step 3: Write minimal implementation**

Create `webhook.go`:

```go
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
func NewWebhookDispatcher(logger *slog.Logger) http.Handler {
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
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestVerifyWebhookSignature|TestParseWebhookPayload|TestWebhookDispatcher" -v`
Expected: PASS (all sub-tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/webhook.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add webhook signature verification, payload parsing, and dispatcher"
```

---

## Task 5: inbound.go â€?Message Normalization

**Files:**
- Create: `server/internal/integrations/rongcloud/inbound.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `NormalizedMessage`, `objectNameText`, `objectNameCommand`, `TypeRongCloud` from Task 1; `channel.InboundMessage` from channel package
- Produces: `normalizeInbound`, `textContent`, `isCommandMessage`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestNormalizeInboundText(t *testing.T) {
	msg := NormalizedMessage{
		ObjectName:       objectNameText,
		FromUserID:       "user1",
		ToUserID:         "sys_node",
		Content:          `{"content":"hello world"}`,
		MsgUID:           "m1",
		MsgTimeStamp:     "1695494400000",
		ConversationType: "1",
	}
	inbound, ok := normalizeInbound(msg)
	if !ok {
		t.Fatal("normalizeInbound returned false")
	}
	if inbound.Type != channel.MsgTypeText {
		t.Errorf("Type = %q, want text", inbound.Type)
	}
	if inbound.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", inbound.Text)
	}
	if inbound.Source.SenderID != "user1" {
		t.Errorf("SenderID = %q", inbound.Source.SenderID)
	}
	if inbound.Source.ChatType != channel.ChatTypeP2P {
		t.Errorf("ChatType = %q, want p2p", inbound.Source.ChatType)
	}
	if inbound.MessageID != "m1" {
		t.Errorf("MessageID = %q", inbound.MessageID)
	}
	if !inbound.AddressedToBot {
		t.Error("AddressedToBot should be true for P2P")
	}
}

func TestNormalizeInboundCommand(t *testing.T) {
	msg := NormalizedMessage{
		ObjectName:       objectNameCommand,
		FromUserID:       "user1",
		ToUserID:         "sys_node",
		Content:          `{"request_id":"r1","service":"ping","action":"ping"}`,
		MsgUID:           "m2",
		ConversationType: "1",
	}
	inbound, ok := normalizeInbound(msg)
	if !ok {
		t.Fatal("normalizeInbound returned false")
	}
	if inbound.MessageID != "m2" {
		t.Errorf("MessageID = %q", inbound.MessageID)
	}
	if inbound.Source.SenderID != "user1" {
		t.Errorf("SenderID = %q", inbound.Source.SenderID)
	}
}

func TestIsCommandMessage(t *testing.T) {
	if !isCommandMessage(objectNameCommand) {
		t.Error("isCommandMessage(command) should be true")
	}
	if isCommandMessage(objectNameText) {
		t.Error("isCommandMessage(RC:TxtMsg) should be false")
	}
}

func TestNormalizeInboundGroupChat(t *testing.T) {
	msg := NormalizedMessage{
		ObjectName:       objectNameText,
		FromUserID:       "user1",
		ToUserID:         "group1",
		Content:          `{"content":"hi"}`,
		MsgUID:           "m3",
		ConversationType: "3",
	}
	inbound, ok := normalizeInbound(msg)
	if !ok {
		t.Fatal("normalizeInbound returned false")
	}
	if inbound.Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("ChatType = %q, want group", inbound.Source.ChatType)
	}
}
```

Add `"github.com/multica-ai/multica/server/internal/integrations/channel"` to the test imports.

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestNormalizeInbound|TestIsCommandMessage" -v`
Expected: FAIL â€?`normalizeInbound`, `isCommandMessage` undefined

- [x] **Step 3: Write minimal implementation**

Create `inbound.go`:

```go
package rongcloud

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// textContent is the JSON structure of a RC:TxtMsg content field.
type textContent struct {
	Content string `json:"content"`
}

// isCommandMessage returns true if the objectName indicates a command message.
func isCommandMessage(objectName string) bool {
	return objectName == objectNameCommand
}

// normalizeInbound converts a RongCloud NormalizedMessage to a channel.InboundMessage.
// Returns ok=false for message types we don't handle (images, typing, etc.).
// The Raw field preserves the original content JSON for protocol-level parsing.
func normalizeInbound(msg NormalizedMessage) (channel.InboundMessage, bool) {
	chatType := channel.ChatTypeP2P
	if msg.ConversationType == "3" || msg.ConversationType == "4" {
		chatType = channel.ChatTypeGroup
	}

	// Determine receiver ID (toUserId with targetId fallback)
	recvID := msg.ToUserID
	if recvID == "" {
		recvID = msg.TargetID
	}

	inbound := channel.InboundMessage{
		MessageID: msg.MsgUID,
		EventID:   msg.MsgUID,
		Source: channel.Source{
			ChannelType: TypeRongCloud,
			ChatID:      recvID,
			ChatType:    chatType,
			SenderID:    msg.FromUserID,
		},
		Raw: json.RawMessage(msg.Content),
	}

	switch msg.ObjectName {
	case objectNameText:
		inbound.Type = channel.MsgTypeText
		var tc textContent
		if err := json.Unmarshal([]byte(msg.Content), &tc); err == nil {
			inbound.Text = tc.Content
		}
	case objectNameCommand:
		inbound.Type = channel.MsgTypeText
		inbound.CommandText = msg.Content
		inbound.SkipAgentRun = true // commands are handled by system_handler, not the agent
	case objectNameImage:
		inbound.Type = channel.MsgTypeImage
	default:
		return channel.InboundMessage{}, false
	}

	// P2P messages are always addressed to the bot (system node).
	// Group messages require explicit mention (not implemented in MVP).
	inbound.AddressedToBot = chatType == channel.ChatTypeP2P

	return inbound, true
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestNormalizeInbound|TestIsCommandMessage" -v`
Expected: PASS (all sub-tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/inbound.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add message normalization to channel.InboundMessage"
```

---

## Task 6: system_handler.go â€?Command Dispatch and Ping

**Files:**
- Create: `server/internal/integrations/rongcloud/system_handler.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `NormalizedMessage`, `CommandContent`, `getString` from Task 1; `rongcloudAPIClient.sendCommandResult` from Task 3
- Produces: `systemHandler`, `newSystemHandler`, `handleCommand`, `handlePing`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestHandleCommandPing(t *testing.T) {
	var receivedRequestID string
	var receivedPayload map[string]interface{}
	var receivedToUser string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("objectName") == "command" {
			var content CommandResultContent
			json.Unmarshal([]byte(r.PostForm.Get("content")), &content)
			receivedRequestID = content.RequestID
			receivedPayload = content.Payload
			receivedToUser = r.PostForm.Get("toUserId")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"req_123","service":"ping","action":"ping","params":{"echo":"hello"}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if receivedRequestID != "req_123" {
		t.Errorf("RequestID = %q, want req_123", receivedRequestID)
	}
	if receivedToUser != "user1" {
		t.Errorf("toUserId = %q, want user1", receivedToUser)
	}
	if receivedPayload["ok"] != true {
		t.Errorf("Payload[ok] = %v, want true", receivedPayload["ok"])
	}
}

func TestHandleCommandUnknownService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"unknown_svc","action":"test","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for unknown service: %v", err)
	}
}

func TestHandleCommandParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `not valid json`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should handle parse error gracefully: %v", err)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestHandleCommand -v`
Expected: FAIL â€?`newSystemHandler`, `handleCommand` undefined

- [x] **Step 3: Write minimal implementation**

Create `system_handler.go`:

```go
package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// systemHandler processes command messages addressed to the system node.
// It dispatches by service name to individual service handlers.
type systemHandler struct {
	client *rongcloudAPIClient
	nodeID string
	logger *slog.Logger
}

func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger) *systemHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &systemHandler{
		client: client,
		nodeID: nodeID,
		logger: logger,
	}
}

// handleCommand parses a command message and dispatches to the appropriate service handler.
// On parse errors or unknown services, it sends an error command_result back to the sender.
func (h *systemHandler) handleCommand(ctx context.Context, msg NormalizedMessage) error {
	var cmd CommandContent
	if err := json.Unmarshal([]byte(msg.Content), &cmd); err != nil {
		h.logger.Error("rongcloud: parse command content", "error", err, "msgUID", msg.MsgUID)
		return h.sendError(ctx, msg, "", "invalid command content")
	}

	// Ignore messages where request_id is missing
	if cmd.RequestID == "" {
		h.logger.Warn("rongcloud: command missing request_id", "msgUID", msg.MsgUID)
		return nil
	}

	switch cmd.Service {
	case "ping":
		return h.handlePing(ctx, msg, cmd)
	default:
		h.logger.Warn("rongcloud: unknown service", "service", cmd.Service, "msgUID", msg.MsgUID)
		return h.sendError(ctx, msg, cmd.RequestID, fmt.Sprintf("unknown service: %s", cmd.Service))
	}
}

// handlePing echoes back a pong response.
func (h *systemHandler) handlePing(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	payload := map[string]interface{}{
		"ok":      true,
		"message": "pong",
	}
	if echo, ok := cmd.Params["echo"]; ok {
		payload["echo"] = echo
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, cmd.RequestID, payload)
}

// sendError sends an error command_result back to the sender.
func (h *systemHandler) sendError(ctx context.Context, msg NormalizedMessage, requestID, errMsg string) error {
	payload := map[string]interface{}{
		"ok":    false,
		"error": errMsg,
	}
	return h.client.sendCommandResult(ctx, h.nodeID, msg.FromUserID, requestID, payload)
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run TestHandleCommand -v`
Expected: PASS (all 3 sub-tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/system_handler.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add system handler with command dispatch and ping"
```

---

## Task 7: channel.go + outbound.go + registration.go â€?Channel Implementation and Factory

**Files:**
- Create: `server/internal/integrations/rongcloud/outbound.go`
- Create: `server/internal/integrations/rongcloud/channel.go`
- Create: `server/internal/integrations/rongcloud/registration.go`
- Test: `server/internal/integrations/rongcloud/rongcloud_test.go`

**Interfaces:**
- Consumes: `credentials`, `decodeCredentials` from Task 2; `rongcloudAPIClient`, `newRongCloudAPIClient` from Task 3; `WebhookRegistrar`, `verifyWebhookSignature`, `parseWebhookPayload` from Task 4; `normalizeInbound`, `isCommandMessage` from Task 5; `systemHandler`, `newSystemHandler` from Task 6; `channel.Channel`, `channel.Config`, `channel.InboundHandler`, `channel.OutboundMessage`, `channel.SendResult`, `channel.Capability` from channel package; `pgtype.UUID` from pgx
- Produces: `rongcloudChannel` (implements `channel.Channel`), `ChannelDeps`, `RegisterRongCloud`, `newRongCloudFactory`, `encodeTextContent`

- [x] **Step 1: Write the failing test**

Append to `rongcloud_test.go`:

```go
func TestEncodeTextContent(t *testing.T) {
	result := encodeTextContent("hello")
	var tc textContent
	if err := json.Unmarshal([]byte(result), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Content != "hello" {
		t.Errorf("Content = %q, want hello", tc.Content)
	}
}

func TestChannelType(t *testing.T) {
	ch := &rongcloudChannel{}
	if ch.Type() != TypeRongCloud {
		t.Errorf("Type() = %q, want %q", ch.Type(), TypeRongCloud)
	}
}

func TestChannelCapabilities(t *testing.T) {
	ch := &rongcloudChannel{}
	caps := ch.Capabilities()
	if !caps.Has(channel.CapText) {
		t.Error("expected CapText capability")
	}
}

func TestChannelConnectRegistersWebhook(t *testing.T) {
	dispatcher := NewWebhookDispatcher(testLogger()).(*webhookDispatcher)

	ch := &rongcloudChannel{
		creds: credentials{
			AppKey:       "key",
			AppSecret:    "secret",
			SystemNodeID: "sys",
		},
		client:  newRongCloudAPIClient("key", "secret", "", nil, testLogger()),
		systemHandler: newSystemHandler(newRongCloudAPIClient("key", "secret", "", nil, testLogger()), "sys", testLogger()),
		registrar: dispatcher,
		handler:  func(ctx context.Context, msg channel.InboundMessage) error { return nil },
		logger:   testLogger(),
	}

	// Use a config ID so the installation ID is non-empty
	ch.cfg.ID = pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ch.Connect(ctx)
	}()

	// Give Connect time to register
	time.Sleep(100 * time.Millisecond)

	// Verify the handler is registered
	dispatcher.mu.RLock()
	_, registered := dispatcher.handlers[ch.cfg.ID.String()]
	dispatcher.mu.RUnlock()
	if !registered {
		t.Fatal("webhook handler not registered")
	}

	// Cancel and verify Connect returns
	cancel()
	err := <-done
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}

	// Verify handler is unregistered on Disconnect
	dispatcher.mu.RLock()
	_, stillRegistered := dispatcher.handlers[ch.cfg.ID.String()]
	dispatcher.mu.RUnlock()
	if stillRegistered {
		t.Fatal("webhook handler still registered after Disconnect")
	}
}

func TestChannelSendText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("objectName") != objectNameText {
			t.Errorf("objectName = %q, want %q", r.PostForm.Get("objectName"), objectNameText)
		}
		var tc textContent
		json.Unmarshal([]byte(r.PostForm.Get("content")), &tc)
		if tc.Content != "hello" {
			t.Errorf("content = %q, want hello", tc.Content)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"msgUID":"sent_001"}`)
	}))
	defer srv.Close()

	ch := &rongcloudChannel{
		creds: credentials{AppKey: "k", AppSecret: "s", SystemNodeID: "sys"},
		client: newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger()),
		logger: testLogger(),
	}
	result, err := ch.Send(context.Background(), channel.OutboundMessage{
		ChatID: "user1",
		Text:   "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.MessageID != "sent_001" {
		t.Errorf("MessageID = %q, want sent_001", result.MessageID)
	}
}

func TestFactory(t *testing.T) {
	dispatcher := NewWebhookDispatcher(testLogger())
	secretB64 := base64.StdEncoding.EncodeToString([]byte("test-secret"))
	raw := json.RawMessage(`{"app_key":"k1","app_secret_encrypted":"` + secretB64 + `","system_node_id":"node1"}`)

	factory := newRongCloudFactory(ChannelDeps{
		Registrar: dispatcher.(WebhookRegistrar),
		Decrypt:   func(b []byte) ([]byte, error) { return b, nil },
		Logger:    testLogger(),
	})

	ch, err := factory(channel.Config{
		Type:    TypeRongCloud,
		Raw:     raw,
		Handler: func(ctx context.Context, msg channel.InboundMessage) error { return nil },
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	rc, ok := ch.(*rongcloudChannel)
	if !ok {
		t.Fatalf("expected *rongcloudChannel, got %T", ch)
	}
	if rc.creds.AppKey != "k1" {
		t.Errorf("AppKey = %q", rc.creds.AppKey)
	}
	if rc.creds.SystemNodeID != "node1" {
		t.Errorf("SystemNodeID = %q", rc.creds.SystemNodeID)
	}
	if rc.handler == nil {
		t.Error("handler not set from cfg.Handler")
	}
}
```

Add `"time"` and `"github.com/jackc/pgx/v5/pgtype"` to the test imports.

- [x] **Step 2: Run test to verify it fails**

Run: `cd server && go test ./internal/integrations/rongcloud/ -run "TestEncodeTextContent|TestChannelType|TestChannelCapabilities|TestChannelConnect|TestChannelSend|TestFactory" -v`
Expected: FAIL â€?`rongcloudChannel`, `encodeTextContent`, `ChannelDeps`, `newRongCloudFactory` undefined

- [x] **Step 3: Write minimal implementation**

Create `outbound.go`:

```go
package rongcloud

import "encoding/json"

// encodeTextContent wraps plain text into the RongCloud RC:TxtMsg content format: {"content":"..."}.
func encodeTextContent(text string) string {
	b, _ := json.Marshal(textContent{Content: text})
	return string(b)
}
```

Create `channel.go`:

```go
package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// rongcloudChannel implements channel.Channel for RongCloud.
// It receives messages via webhook (registered with a WebhookRegistrar) and
// sends messages via the RongCloud Server REST API.
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

// Connect registers the webhook handler and blocks until the context is cancelled.
// This is the first webhook-based IM adapter â€?Connect does not run a receive loop;
// instead, the webhook handler is called by the HTTP server on incoming requests.
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
	return ctx.Err()
}

// Disconnect unregisters the webhook handler. Safe to call multiple times.
func (c *rongcloudChannel) Disconnect(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	installationID := c.installationID()
	c.registrar.Unregister(installationID)
	c.logger.Info("rongcloud channel disconnected", "installation_id", installationID)
	return nil
}

// Send sends a text message to the specified chat (user ID).
func (c *rongcloudChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	content := encodeTextContent(out.Text)
	msgUID, err := c.client.sendPrivateMessage(ctx, c.creds.SystemNodeID, out.ChatID, objectNameText, content)
	if err != nil {
		return channel.SendResult{}, fmt.Errorf("rongcloud: send: %w", err)
	}
	return channel.SendResult{MessageID: msgUID}, nil
}

// handleWebhook is the HTTP handler registered with the WebhookDispatcher.
// It verifies the signature, parses the payload, and routes command messages
// to the system handler or non-command messages to the engine handler.
func (c *rongcloudChannel) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Verify webhook signature
	if !verifyWebhookSignature(c.creds.AppSecret, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse webhook payload
	msg, err := parseWebhookPayload(r)
	if err != nil {
		c.logger.Error("rongcloud: parse webhook payload", "error", err)
		w.WriteHeader(http.StatusOK) // fire-and-classify: discard unparseable
		return
	}

	// Dedup by msgUID (fallback to from_to_timestamp)
	msgID := msg.MsgUID
	if msgID == "" {
		msgID = fmt.Sprintf("%s_%s_%s", msg.FromUserID, msg.ToUserID, msg.MsgTimeStamp)
	}

	// Route command messages to system handler
	if isCommandMessage(msg.ObjectName) {
		if err := c.systemHandler.handleCommand(c.ctx, msg); err != nil {
			c.logger.Error("rongcloud: handle command", "error", err, "msgUID", msgID)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Normalize and pass to engine handler
	inbound, ok := normalizeInbound(msg)
	if !ok {
		w.WriteHeader(http.StatusOK) // fire-and-classify: discard unsupported types
		return
	}

	if err := c.handler(c.ctx, inbound); err != nil {
		c.logger.Error("rongcloud: inbound handler", "error", err, "msgUID", msgID)
		w.WriteHeader(http.StatusInternalServerError) // infrastructure error â†?RongCloud retries
		return
	}
	w.WriteHeader(http.StatusOK)
}

// installationID returns the string form of the channel installation UUID.
func (c *rongcloudChannel) installationID() string {
	if c.cfg.ID.Valid {
		return c.cfg.ID.String()
	}
	return "default"
}
```

Create `registration.go`:

```go
package rongcloud

import (
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// ChannelDeps holds dependencies injected at registration time.
type ChannelDeps struct {
	Registrar  WebhookRegistrar
	Decrypt    Decrypter
	Logger     *slog.Logger
	APIBase    string
	HTTPClient *http.Client
}

// RegisterRongCloud registers the RongCloud channel factory with the registry.
func RegisterRongCloud(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeRongCloud, newRongCloudFactory(deps))
}

// newRongCloudFactory returns a channel.Factory that creates rongcloudChannel instances.
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
```

- [x] **Step 4: Run test to verify it passes**

Run: `cd server && go test ./internal/integrations/rongcloud/ -v`
Expected: ALL tests PASS (including previous tasks' tests and new Task 7 tests)

- [x] **Step 5: Commit**

```bash
git add server/internal/integrations/rongcloud/outbound.go server/internal/integrations/rongcloud/channel.go server/internal/integrations/rongcloud/registration.go server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "feat(rongcloud): add Channel implementation, factory, and registration"
```

---

## Task 8: router.go â€?Wiring

**Files:**
- Modify: `server/cmd/server/router.go`

**Interfaces:**
- Consumes: `RegisterRongCloud`, `NewWebhookDispatcher`, `ChannelDeps`, `TypeRongCloud` from Task 7; existing `channelRegistry`, `channelRouter`, `secretbox` from router.go

This task has no automated test â€?it is manual wiring verified by compilation.

- [x] **Step 1: Add the import**

In `server/cmd/server/router.go`, add to the import block (near the other integration imports, around line 28-35):

```go
"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
```

- [x] **Step 2: Add the RongCloud registration block**

Find the Telegram registration block (around lines 1158-1206). Immediately after it (and its closing `}`), add:

```go
	// RongCloud (èžäº‘) integration â€?env-gated by MULTICA_RONGCLOUD_SECRET_KEY.
	// This is the first webhook-based IM adapter; inbound messages arrive via
	// POST /api/webhooks/rongcloud?inst={installation_id}.
	if rongcloudKey, err := secretbox.LoadKey("MULTICA_RONGCLOUD_SECRET_KEY"); err == nil {
		rcBox, err := secretbox.New(rongcloudKey)
		if err != nil {
			slog.Error("rongcloud integration failed to init secretbox", "error", err)
		} else {
			rcWebhookDispatcher := rongcloud.NewWebhookDispatcher(slog.Default())
			r.Post("/api/webhooks/rongcloud", rcWebhookDispatcher)
			rongcloud.RegisterRongCloud(channelRegistry, rongcloud.ChannelDeps{
				Registrar: rcWebhookDispatcher.(rongcloud.WebhookRegistrar),
				Decrypt:   rcBox.Open,
				Logger:    slog.Default(),
			})
			slog.Info("rongcloud integration enabled")
		}
	} else {
		slog.Info("rongcloud integration disabled (MULTICA_RONGCLOUD_SECRET_KEY not set)")
	}
```

**Important:** The `r.Post("/api/webhooks/rongcloud", rcWebhookDispatcher)` line must be placed in the route registration section (near line 1487-1508 where other webhook routes are). However, since the dispatcher is created inside the env-gated block, the route registration must also be inside that block. If the route registration section is separate from the adapter registration section (which it is in this codebase â€?routes are registered in a different function/block), then:

1. Create the dispatcher before the route section and store it in a variable accessible to both blocks, OR
2. Register the route inside the env-gated block.

Check the actual code structure: if `r` (the Chi router) is available in the same scope as the Telegram registration block, then option 2 works. If `r` is only available later in a separate route-registration block, use a package-level variable or pass the dispatcher to a closure.

**Recommended approach:** If `r` is available at the registration block scope (it is â€?the Telegram block uses `channelRouter.Register` which is defined just above), then add the `r.Post` line inside the env-gated block as shown above. If not, hoist `rcWebhookDispatcher` to a variable declared before the env-gated block and register the route in the webhook routes section.

- [x] **Step 3: Verify compilation**

Run: `cd server && go build ./cmd/server/`
Expected: compiles with no errors

- [x] **Step 4: Run full test suite**

Run: `cd server && make test`
Expected: all existing tests pass + new rongcloud tests pass

- [x] **Step 5: Commit**

```bash
git add server/cmd/server/router.go
git commit -m "feat(rongcloud): wire RongCloud adapter into router with webhook route"
```

---

## Self-Review

### 1. Spec coverage

| Spec section | Covered by |
|---|---|
| æž¶æž„æ¦‚è§ˆ (Architecture overview) | All 8 tasks build the adapter in `server/internal/integrations/rongcloud/` |
| Channel æŽ¥å£å®žçŽ° (Channel interface) | Task 7: `rongcloudChannel` implements all 5 methods |
| system èŠ‚ç‚¹ä¸šåŠ¡é€»è¾‘ (system node logic) | Task 6: `systemHandler` with ping dispatch |
| æ•°æ®æµä¸Žæ¶ˆæ¯ç”Ÿå‘½å‘¨æœŸ (Data flow) | Task 7: `handleWebhook` routes commandâ†’system_handler, non-commandâ†’engine handler |
| é”™è¯¯å¤„ç†ä¸Žé‡è¿?(Error handling) | Task 7: signature failureâ†?01, parse failureâ†?00, handler errorâ†?00, system errorâ†?00 |
| æµ‹è¯•ç­–ç•¥ (Testing strategy) | All tasks use httptest + direct struct construction, matching Telegram pattern |
| Webhook + REST API è¿žæŽ¥æ¨¡åž‹ | Task 4 (webhook), Task 3 (REST), Task 7 (Connect/Disconnect lifecycle) |
| commandâ†’command_result RPC å›žçŽ¯ (MVP ping) | Task 6: `handlePing` sends command_result with pong payload |
| åŒ…ç»“æž?(Package structure) | 9 files matching spec's file structure |
| æœ€å°æºç ä¿®æ”?(Minimal source changes) | Task 8: only router.go modified (import + registration block + webhook route) |

### 2. Placeholder scan

No TBD, TODO, or vague steps found. All code blocks contain actual implementation code.

### 3. Type consistency

- `NormalizedMessage` â€?defined Task 1, used Tasks 4-7 âœ?- `CommandContent` â€?defined Task 1, used Task 6 âœ?- `CommandResultContent` â€?defined Task 1, used Task 3 âœ?- `rongcloudAPIClient` â€?defined Task 3, used Tasks 6-7 âœ?- `systemHandler` / `newSystemHandler` â€?defined Task 6, used Task 7 âœ?- `WebhookRegistrar` â€?defined Task 4, used Task 7 âœ?- `rongcloudChannel` â€?defined Task 7 âœ?- `ChannelDeps` â€?defined Task 7, used Task 8 âœ?- `encodeTextContent` â€?defined Task 7 (outbound.go), used Task 7 (channel.go Send) âœ?- `isCommandMessage` â€?defined Task 5, used Task 7 âœ?- `normalizeInbound` â€?defined Task 5, used Task 7 âœ?- `getString` â€?defined Task 1, used Task 4 âœ?- `objectNameText` / `objectNameCommand` â€?defined Task 1, used Tasks 3-7 âœ?- `computeSignature` / `computeSignatureFromString` â€?defined Task 3, used Task 4 âœ?- `decodeCredentials` â€?defined Task 2, used Task 7 âœ?- `decryptSecret` â€?defined Task 2, used internally in Task 2 âœ?- `verifyWebhookSignature` â€?defined Task 4, used Task 7 âœ?- `parseWebhookPayload` â€?defined Task 4, used Task 7 âœ?