package rongcloud

import (
	"encoding/json"
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
