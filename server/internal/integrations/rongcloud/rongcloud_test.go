package rongcloud

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestComputeSignature(t *testing.T) {
	sig := computeSignature("my-secret", "abc123nonce456", 1695494400000)
	if len(sig) != 40 {
		t.Fatalf("signature length = %d, want 40", len(sig))
	}
	sig2 := computeSignature("my-secret", "abc123nonce456", 1695494400000)
	if sig != sig2 {
		t.Fatalf("signature not deterministic: %q vs %q", sig, sig2)
	}
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
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

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
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

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
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

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
	dispatcher := NewWebhookDispatcher(testLogger())

	ch := &rongcloudChannel{
		creds: credentials{
			AppKey:       "key",
			AppSecret:    "secret",
			SystemNodeID: "sys",
		},
		client:       newRongCloudAPIClient("key", "secret", "", nil, testLogger()),
		systemHandler: newSystemHandler(newRongCloudAPIClient("key", "secret", "", nil, testLogger()), "sys", testLogger(), nil, nil),
		registrar:     dispatcher,
		handler:      func(ctx context.Context, msg channel.InboundMessage) error { return nil },
		logger:        testLogger(),
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
		creds:  credentials{AppKey: "k", AppSecret: "s", SystemNodeID: "sys"},
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
		Registrar: dispatcher,
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

// newTestWebhookChannel builds a rongcloudChannel wired with a mock RongCloud API
// server and a capturing InboundHandler, suitable for handleWebhook integration tests.
func newTestWebhookChannel(t *testing.T) (*rongcloudChannel, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))

	dispatcher := NewWebhookDispatcher(testLogger())
	ch := &rongcloudChannel{
		creds: credentials{
			AppKey:       "test-key",
			AppSecret:    "test-secret",
			SystemNodeID: "sys-node",
		},
		client:        newRongCloudAPIClient("test-key", "test-secret", srv.URL, srv.Client(), testLogger()),
		systemHandler:  newSystemHandler(newRongCloudAPIClient("test-key", "test-secret", srv.URL, srv.Client(), testLogger()), "sys-node", testLogger(), nil, nil),
		registrar:      dispatcher,
		logger:         testLogger(),
	}
	if err := dispatcher.Register("test-inst", ch.handleWebhook); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	return ch, srv
}

// buildTextWebhookRequest constructs a form-urlencoded RongCloud text-message
// webhook request with a valid (or overrideable) signature.
func buildTextWebhookRequest(t *testing.T, appSecret, signatureOverride string) *http.Request {
	t.Helper()
	form := url.Values{
		"objectName":       {"RC:TxtMsg"},
		"fromUserId":       {"user1"},
		"toUserId":         {"sys-node"},
		"msgUID":           {"msg-1"},
		"msgTimeStamp":     {"1234567890"},
		"conversationType": {"1"},
		"content":          {`{"content":"hello world"}`},
	}
	body := form.Encode()
	req := httptest.NewRequest(http.MethodPost, "http://localhost/webhooks/rongcloud?inst=test-inst", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	nonce := "nonce-for-test-1"
	timestamp := "1234567890"
	sig := computeSignatureFromString(appSecret, nonce, timestamp)
	if signatureOverride != "" {
		sig = signatureOverride
	}
	req.Header.Set("rc-nonce", nonce)
	req.Header.Set("rc-timestamp", timestamp)
	req.Header.Set("rc-signature", sig)
	return req
}

func TestHandleWebhookText(t *testing.T) {
	ch, srv := newTestWebhookChannel(t)
	defer srv.Close()

	var captured channel.InboundMessage
	var invoked bool
	ch.handler = func(ctx context.Context, msg channel.InboundMessage) error {
		captured = msg
		invoked = true
		return nil
	}

	req := buildTextWebhookRequest(t, "test-secret", "")
	rr := httptest.NewRecorder()
	ch.handleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if !invoked {
		t.Fatal("inbound handler was not invoked")
	}
	if captured.Text != "hello world" {
		t.Errorf("Text = %q, want %q", captured.Text, "hello world")
	}
	if captured.Source.SenderID != "user1" {
		t.Errorf("SenderID = %q, want %q", captured.Source.SenderID, "user1")
	}
	if captured.Source.ChatID != "sys-node" {
		t.Errorf("ChatID = %q, want %q", captured.Source.ChatID, "sys-node")
	}
}

func TestHandleWebhookBadSignature(t *testing.T) {
	ch, srv := newTestWebhookChannel(t)
	defer srv.Close()

	var invoked bool
	ch.handler = func(ctx context.Context, msg channel.InboundMessage) error {
		invoked = true
		return nil
	}

	req := buildTextWebhookRequest(t, "test-secret", "deadbeef")
	rr := httptest.NewRecorder()
	ch.handleWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if invoked {
		t.Fatal("inbound handler should not be invoked for bad signature")
	}
}

func writeJSONResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestGetUserToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/getToken.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("userId") != "user1" {
			t.Errorf("userId: got %q", r.PostForm.Get("userId"))
		}
		if r.PostForm.Get("name") != "Alice" {
			t.Errorf("name: got %q", r.PostForm.Get("name"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200, "token": "token-abc"})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	token, err := c.getUserToken(context.Background(), "user1", "Alice", "http://avatar.png")
	if err != nil {
		t.Fatalf("getUserToken: %v", err)
	}
	if token != "token-abc" {
		t.Errorf("token: got %q want %q", token, "token-abc")
	}
}

func TestCheckOnline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]interface{}{"code": 200, "status": "1"})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	online, err := c.checkOnline(context.Background(), "user1")
	if err != nil {
		t.Fatalf("checkOnline: %v", err)
	}
	if !online {
		t.Error("expected online=true")
	}
}

func TestGetUserInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]interface{}{
			"code":        200,
			"userId":      "user1",
			"name":        "Alice",
			"portraitUri": "http://avatar.png",
		})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	info, err := c.getUserInfo(context.Background(), "user1")
	if err != nil {
		t.Fatalf("getUserInfo: %v", err)
	}
	if info.UserID != "user1" || info.Name != "Alice" || info.PortraitURI != "http://avatar.png" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestCreateChatroom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chatroom/create_new.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("chatroomId") != "room1" {
			t.Errorf("chatroomId: got %q", r.PostForm.Get("chatroomId"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.createChatroom(context.Background(), "room1"); err != nil {
		t.Fatalf("createChatroom: %v", err)
	}
}

func TestJoinChatroom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("chatroomId") != "room1" {
			t.Errorf("chatroomId: got %q", r.PostForm.Get("chatroomId"))
		}
		if r.PostForm.Get("userId") != "user1,user2" {
			t.Errorf("userId: got %q", r.PostForm.Get("userId"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.joinChatroom(context.Background(), "room1", []string{"user1", "user2"}); err != nil {
		t.Fatalf("joinChatroom: %v", err)
	}
}

func TestGetChatroomInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]interface{}{
			"code":       200,
			"chatRoomId": "room1",
			"name":       "Test Room",
		})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	info, err := c.getChatroomInfo(context.Background(), "room1")
	if err != nil {
		t.Fatalf("getChatroomInfo: %v", err)
	}
	if info.ChatroomID != "room1" || info.Name != "Test Room" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestEnsureChatroom(t *testing.T) {
	createCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chatroom/get.json":
			writeJSONResponse(w, map[string]interface{}{"code": 23410})
		case "/chatroom/create_new.json":
			createCalled = true
			writeJSONResponse(w, map[string]interface{}{"code": 200})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.ensureChatroom(context.Background(), "room1", "Test Room"); err != nil {
		t.Fatalf("ensureChatroom: %v", err)
	}
	if !createCalled {
		t.Error("expected createChatroom to be called")
	}
}

func TestCreateGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/group/create.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("groupId") != "grp1" {
			t.Errorf("groupId: got %q", r.PostForm.Get("groupId"))
		}
		if r.PostForm.Get("groupName") != "Team" {
			t.Errorf("groupName: got %q", r.PostForm.Get("groupName"))
		}
		// userId is a repeated param
		uids := r.PostForm["userId"]
		if len(uids) != 2 || uids[0] != "u1" || uids[1] != "u2" {
			t.Errorf("userId: got %v", uids)
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.createGroup(context.Background(), "grp1", "Team", []string{"u1", "u2"}); err != nil {
		t.Fatalf("createGroup: %v", err)
	}
}

func TestDismissGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("groupId") != "grp1" {
			t.Errorf("groupId: got %q", r.PostForm.Get("groupId"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.dismissGroup(context.Background(), "grp1", "u1"); err != nil {
		t.Fatalf("dismissGroup: %v", err)
	}
}

func TestRefreshGroupInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("groupId") != "grp1" {
			t.Errorf("groupId: got %q", r.PostForm.Get("groupId"))
		}
		if r.PostForm.Get("groupName") != "New Name" {
			t.Errorf("groupName: got %q", r.PostForm.Get("groupName"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.refreshGroupInfo(context.Background(), "grp1", "New Name", ""); err != nil {
		t.Fatalf("refreshGroupInfo: %v", err)
	}
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func TestNewInstallService(t *testing.T) {
	svc := NewInstallService(nil, nil, testLogger())
	if svc == nil {
		t.Fatal("expected non-nil InstallService")
	}
}

func TestInstallServiceGetAppKey(t *testing.T) {
	box := testBox(t)
	svc := NewInstallService(nil, box, testLogger())
	key := svc.GetAppKey(context.Background())
	if key != "" {
		t.Errorf("expected empty appKey without DB, got %q", key)
	}
}

func TestVerifyEnrollmentToken(t *testing.T) {
	svc := NewNodeService(nil, nil, nil, testLogger())
	secret := "bridge-secret"
	serverURL := "http://localhost:8080"
	runtimeID := "runtime-1"
	token := svc.enrollmentToken(secret, serverURL, runtimeID)
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if !svc.VerifyEnrollmentToken(token, secret, serverURL, runtimeID) {
		t.Error("expected token to verify")
	}
	if svc.VerifyEnrollmentToken("wrong-token", secret, serverURL, runtimeID) {
		t.Error("expected wrong token to fail verification")
	}
}

func TestNodeServiceEnrollDeviceCredential(t *testing.T) {
	credID, secret, err := generateDeviceCredential()
	if err != nil {
		t.Fatalf("generateDeviceCredential: %v", err)
	}
	if !strings.HasPrefix(credID, "dc_") {
		t.Errorf("credential ID should start with dc_, got %q", credID)
	}
	if len(secret) == 0 {
		t.Error("expected non-empty secret")
	}
}

func TestNewChatroomService(t *testing.T) {
	svc := NewChatroomService(nil, nil, testLogger())
	if svc == nil {
		t.Fatal("expected non-nil ChatroomService")
	}
}

func TestChatroomServiceEnsureChatroom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chatroom/get.json":
			writeJSONResponse(w, map[string]interface{}{"code": 23410})
		case "/chatroom/create_new.json":
			writeJSONResponse(w, map[string]interface{}{"code": 200})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	svc := NewChatroomService(nil, c, testLogger())
	err := svc.ensureRongCloudChatroom(context.Background(), "room1", "Test Room")
	if err != nil {
		t.Fatalf("ensureRongCloudChatroom: %v", err)
	}
}

func TestNewPairingService(t *testing.T) {
	svc := NewPairingService(nil, nil, testLogger())
	if svc == nil {
		t.Fatal("expected non-nil PairingService")
	}
}

func TestPairingServiceGenerateTicket(t *testing.T) {
	svc := NewPairingService(nil, nil, testLogger())
	ticket := svc.generateTicket()
	if len(ticket) < 16 {
		t.Errorf("ticket too short: %q", ticket)
	}
}

func TestHandleNodeMessage(t *testing.T) {
	sh := newSystemHandler(nil, "node-123", testLogger(), nil, nil)
	msg := NormalizedMessage{
		MsgUID:      "msg-001",
		FromUserID:  "user-abc",
		ObjectName:  "RC:TxtMsg",
		Content:     `{"text":"hello"}`,
	}
	sh.handleNodeMessage(context.Background(), msg)
}

func TestNewRongCloudAPIClientForServices(t *testing.T) {
	client := NewRongCloudAPIClientForServices(nil, nil, testLogger())
	if client == nil {
		t.Fatal("expected non-nil rongcloudAPIClient")
	}
}

func TestParseUUIDParam(t *testing.T) {
	uuidStr := "550e8400-e29b-41d4-a716-446655440000"
	u, err := parseUUIDParam(uuidStr)
	if err != nil {
		t.Fatalf("parseUUIDParam: %v", err)
	}
	if !u.Valid {
		t.Error("expected Valid=true")
	}
}

func TestParseUUIDParamEmpty(t *testing.T) {
	_, err := parseUUIDParam("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

func TestParseUUIDParamInvalid(t *testing.T) {
	_, err := parseUUIDParam("not-a-uuid")
	if err == nil {
		t.Fatal("expected error for invalid uuid")
	}
}

func TestParseWorkspaceFromSessionIDValid(t *testing.T) {
	wsHex := "550e8400e29b41d4a716446655440000"
	sessionID := "sess_" + wsHex + "_abc123def"
	u, ok := parseWorkspaceFromSessionID(sessionID)
	if !ok {
		t.Fatalf("expected ok=true for sessionID %q", sessionID)
	}
	if !u.Valid {
		t.Error("expected Valid=true")
	}
	expected := [16]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00}
	if u.Bytes != expected {
		t.Errorf("workspace bytes mismatch: got %v", u.Bytes)
	}
}

func TestParseWorkspaceFromSessionIDTooShort(t *testing.T) {
	_, ok := parseWorkspaceFromSessionID("sess_short")
	if ok {
		t.Fatal("expected ok=false for short sessionID")
	}
}

func TestParseWorkspaceFromSessionIDBadPrefix(t *testing.T) {
	wsHex := "550e8400e29b41d4a716446655440000"
	_, ok := parseWorkspaceFromSessionID("bad_" + wsHex + "_abc")
	if ok {
		t.Fatal("expected ok=false for bad prefix")
	}
}

func TestParseWorkspaceFromSessionIDBadHex(t *testing.T) {
	_, ok := parseWorkspaceFromSessionID("sess_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz_abc")
	if ok {
		t.Fatal("expected ok=false for invalid hex workspace")
	}
}

func TestParseWorkspaceFromSessionIDMissingSeparator(t *testing.T) {
	wsHex := "550e8400e29b41d4a716446655440000"
	_, ok := parseWorkspaceFromSessionID("sess_" + wsHex + "abc")
	if ok {
		t.Fatal("expected ok=false for missing separator")
	}
}

func TestHandleChatroomCommandNilQueries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"chatroom","action":"list","params":{"workspace_id":"550e8400-e29b-41d4-a716-446655440000"}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for nil queries: %v", err)
	}
}

func TestHandleChatroomCommandUnknownAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"chatroom","action":"unknown","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for unknown action: %v", err)
	}
}

func TestHandleDeviceCommandNilQueries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"device","action":"list","params":{"workspace_id":"550e8400-e29b-41d4-a716-446655440000"}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for nil queries: %v", err)
	}
}

func TestHandleDeviceCommandUnknownAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"device","action":"unknown","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for unknown action: %v", err)
	}
}

func TestHandleChatroomCommandInvalidWorkspaceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"chatroom","action":"list","params":{"workspace_id":"not-a-uuid"}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for invalid workspace_id: %v", err)
	}
}

func TestHandleChatroomCommandMissingWorkspaceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"chatroom","action":"list","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error for missing workspace_id: %v", err)
	}
}

func TestNewPairingServiceWithBox(t *testing.T) {
	box := testBox(t)
	svc := NewPairingService(nil, box, testLogger())
	if svc == nil {
		t.Fatal("expected non-nil PairingService")
	}
}

func TestDestroyChatroomAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chatroom/destroy.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("chatroomId") != "room1" {
			t.Errorf("chatroomId: got %q", r.PostForm.Get("chatroomId"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.destroyChatroom(context.Background(), "room1"); err != nil {
		t.Fatalf("destroyChatroom: %v", err)
	}
}

func TestQuitChatroomAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chatroom/quit.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostForm.Get("chatroomId") != "room1" {
			t.Errorf("chatroomId: got %q", r.PostForm.Get("chatroomId"))
		}
		if r.PostForm.Get("userId") != "user1,user2" {
			t.Errorf("userId: got %q", r.PostForm.Get("userId"))
		}
		writeJSONResponse(w, map[string]interface{}{"code": 200})
	}))
	defer srv.Close()
	c := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	if err := c.quitChatroom(context.Background(), "room1", []string{"user1", "user2"}); err != nil {
		t.Fatalf("quitChatroom: %v", err)
	}
}

// --- Phase 3 Tests ---

func TestReplayStateEmpty(t *testing.T) {
	state, err := ReplayState(nil)
	if err != nil {
		t.Fatalf("ReplayState: %v", err)
	}
	if state.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", state.Status, StatusIdle)
	}
}

func TestReplayStateFullDiscussion(t *testing.T) {
	wsID := pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}
	crID := pgtype.UUID{Bytes: [16]byte{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}, Valid: true}
	nodeID := pgtype.UUID{Bytes: [16]byte{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}, Valid: true}

	events := []DiscussionEvent{
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventDiscussionStarted, CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventRoundStarted, RoundNumber: 1, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(1 * time.Second), Valid: true}},
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventTurnStarted, RoundNumber: 1, SpeakingOrder: 1, NodeID: nodeID, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(2 * time.Second), Valid: true}},
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventTurnCompleted, RoundNumber: 1, SpeakingOrder: 1, NodeID: nodeID, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(3 * time.Second), Valid: true}},
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventRoundCompleted, RoundNumber: 1, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(4 * time.Second), Valid: true}},
		{ChatroomID: crID, WorkspaceID: wsID, EventType: EventDiscussionEnded, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Second), Valid: true}},
	}
	state, err := ReplayState(events)
	if err != nil {
		t.Fatalf("ReplayState: %v", err)
	}
	if state.Status != StatusEnded {
		t.Errorf("Status = %q, want %q", state.Status, StatusEnded)
	}
	if state.CurrentRound != 1 {
		t.Errorf("CurrentRound = %d, want 1", state.CurrentRound)
	}
	if state.ChatroomID != crID {
		t.Errorf("ChatroomID mismatch")
	}
}

func TestValidateTransitionValid(t *testing.T) {
	newStatus, err := ValidateTransition(StatusIdle, EventDiscussionStarted)
	if err != nil {
		t.Fatalf("ValidateTransition: %v", err)
	}
	if newStatus != StatusStarting {
		t.Errorf("newStatus = %q, want %q", newStatus, StatusStarting)
	}
}

func TestValidateTransitionInvalid(t *testing.T) {
	_, err := ValidateTransition(StatusEnded, EventDiscussionStarted)
	if err == nil {
		t.Fatal("expected error for invalid transition from ended to started")
	}
}

func TestElectHostSkipFailed(t *testing.T) {
	node1 := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	node2 := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	speakers := []SpeakerInfo{
		{NodeID: node1, SpeakingOrder: 1},
		{NodeID: node2, SpeakingOrder: 2},
	}
	host, err := ElectHost(speakers, node1)
	if err != nil {
		t.Fatalf("ElectHost: %v", err)
	}
	if host != node2 {
		t.Errorf("expected node2, got different UUID")
	}
}

func TestElectHostNoCandidate(t *testing.T) {
	node1 := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	speakers := []SpeakerInfo{{NodeID: node1, SpeakingOrder: 1}}
	_, err := ElectHost(speakers, node1)
	if err == nil {
		t.Fatal("expected error when no candidates available")
	}
}

func TestStreamAssemblerComplete(t *testing.T) {
	sa := NewStreamAssembler()
	sa.StartStream("turn1", "node1")
	if err := sa.AppendChunk("turn1", "chunk1 "); err != nil {
		t.Fatalf("AppendChunk: %v", err)
	}
	if err := sa.AppendChunk("turn1", "chunk2"); err != nil {
		t.Fatalf("AppendChunk: %v", err)
	}
	result, err := sa.CompleteStream("turn1")
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result != "chunk1 chunk2" {
		t.Errorf("result = %q, want %q", result, "chunk1 chunk2")
	}
}

func TestStreamAssemblerAbort(t *testing.T) {
	sa := NewStreamAssembler()
	sa.StartStream("turn1", "node1")
	sa.AppendChunk("turn1", "chunk1")
	sa.AbortStream("turn1")
	_, err := sa.CompleteStream("turn1")
	if err == nil {
		t.Fatal("expected error after abort")
	}
}

func TestDiscussionRegistryPutGetDelete(t *testing.T) {
	reg := NewDiscussionRegistry()
	crID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	coordinator := &DiscussionCoordinator{chatroomID: crID}
	reg.Put(crID, coordinator)
	got, ok := reg.Get(crID)
	if !ok {
		t.Fatal("expected coordinator to be found")
	}
	if got != coordinator {
		t.Error("got wrong coordinator")
	}
	reg.Delete(crID)
	_, ok = reg.Get(crID)
	if ok {
		t.Fatal("expected coordinator to be deleted")
	}
}

func TestDiscussionRegistrySendResponseNoCoordinator(t *testing.T) {
	reg := NewDiscussionRegistry()
	crID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	ok := reg.SendResponse(crID, NodeResponse{MsgType: "text"})
	if ok {
		t.Fatal("expected false for non-existent coordinator")
	}
}

func TestHandleDiscussionCommandUnknownAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"discussion","action":"unknown","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error: %v", err)
	}
}

func TestHandleDiscussionCommandStatusNoRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"discussion","action":"status","params":{"chatroom_id":"550e8400-e29b-41d4-a716-446655440000"}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error: %v", err)
	}
}

func TestHandleNodeMessageNilRegistry(t *testing.T) {
	sh := newSystemHandler(nil, "node-123", testLogger(), nil, nil)
	msg := NormalizedMessage{
		MsgUID:      "msg-001",
		FromUserID:  "user-abc",
		ObjectName:  objectNameStream,
		Content:     `{"turn_id":"turn1","stream_type":"chunk","content":"hi"}`,
	}
	sh.handleNodeMessage(context.Background(), msg)
}

func TestIsCommandResult(t *testing.T) {
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		Content:    `{"request_id":"r1","msg_type":"command_result","payload":{"ok":true}}`,
	}
	if !isCommandResult(msg) {
		t.Error("expected isCommandResult=true for command_result message")
	}
	msg2 := NormalizedMessage{
		ObjectName: objectNameCommand,
		Content:    `{"request_id":"r1","service":"ping","action":"ping"}`,
	}
	if isCommandResult(msg2) {
		t.Error("expected isCommandResult=false for non-command_result message")
	}
}

func TestIsStreamMessage(t *testing.T) {
	if !isStreamMessage(objectNameStream) {
		t.Error("expected isStreamMessage=true for RC:StreamMsg")
	}
	if isStreamMessage(objectNameText) {
		t.Error("expected isStreamMessage=false for RC:TxtMsg")
	}
}

func TestNewDiscussionBridge(t *testing.T) {
	b := NewDiscussionBridge(nil, testLogger())
	if b == nil {
		t.Fatal("expected non-nil DiscussionBridge")
	}
}

func TestDiscussionBridgeIsServerManagedByTypeEmpty(t *testing.T) {
	b := NewDiscussionBridge(nil, testLogger())
	if b.IsServerManagedByType("") {
		t.Error("expected IsServerManagedByType(\"\") = false")
	}
}

func TestDiscussionBridgeIsServerManagedByTypeUnknown(t *testing.T) {
	b := NewDiscussionBridge(nil, testLogger())
	if b.IsServerManagedByType("nonexistent-agent-xyz-123") {
		t.Error("expected IsServerManagedByType for unknown agent = false")
	}
}

func TestDiscussionBridgeIsServerManagedInvalidAiType(t *testing.T) {
	b := NewDiscussionBridge(nil, testLogger())
	node := db.RongcloudNode{
		AiType: pgtype.Text{String: "", Valid: false},
	}
	if b.IsServerManaged(node) {
		t.Error("expected IsServerManaged with invalid AiType = false")
	}
}

func TestHandleDiscussionCommandYourTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200}`)
	}))
	defer srv.Close()

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	registry := NewDiscussionRegistry()
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, registry)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user1",
		Content:    `{"request_id":"r1","service":"discussion","action":"your_turn","params":{}}`,
	}
	err := handler.handleCommand(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCommand should not return error: %v", err)
	}
}

func TestNewDiscussionServiceWithBridge(t *testing.T) {
	svc := NewDiscussionService(nil, nil, nil, nil, testLogger())
	if svc == nil {
		t.Fatal("expected non-nil DiscussionService")
	}
}

func TestNewDiscussionCoordinatorWithBridge(t *testing.T) {
	state := DiscussionState{
		Status:     StatusInProgress,
		CurrentRound: 1,
	}
	coord := NewDiscussionCoordinator(
		pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		state,
		nil, nil, nil, nil, testLogger(),
	)
	if coord == nil {
		t.Fatal("expected non-nil DiscussionCoordinator")
	}
}
