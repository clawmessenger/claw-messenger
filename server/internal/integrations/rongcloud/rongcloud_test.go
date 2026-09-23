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
