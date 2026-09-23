package rongcloud

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
