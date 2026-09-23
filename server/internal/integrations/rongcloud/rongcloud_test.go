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

	"github.com/multica-ai/multica/server/internal/integrations/channel"
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
