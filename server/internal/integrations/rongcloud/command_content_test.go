package rongcloud

import (
	"encoding/json"
	"testing"
)

// CommandContent 必须同时吃下两种拼写：设备 CLI 走 snake_case
// （{"request_id","service","action","params"}），C 端 web 走 camelCase
// （{"requestId","service","action","payload"}）。此前只认前者，导致 web 发的
// 每一条系统命令都在 request_id 为空时被静默丢弃。
func TestCommandContentAcceptsBothSpellings(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantID     string
		wantAction string
		wantParam  string
	}{
		{
			name:       "device snake_case",
			raw:        `{"request_id":"app-1","service":"group","action":"create","params":{"groupName":"G"}}`,
			wantID:     "app-1",
			wantAction: "create",
			wantParam:  "G",
		},
		{
			name:       "web camelCase payload",
			raw:        `{"requestId":"web-1","service":"group","action":"create","payload":{"groupName":"G"}}`,
			wantID:     "web-1",
			wantAction: "create",
			wantParam:  "G",
		},
		{
			name:       "camelCase params key",
			raw:        `{"requestId":"web-2","service":"group","action":"update","params":{"groupName":"H"}}`,
			wantID:     "web-2",
			wantAction: "update",
			wantParam:  "H",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cmd CommandContent
			if err := json.Unmarshal([]byte(tc.raw), &cmd); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cmd.RequestID != tc.wantID {
				t.Errorf("RequestID = %q, want %q", cmd.RequestID, tc.wantID)
			}
			if cmd.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", cmd.Action, tc.wantAction)
			}
			if got := getString(cmd.Params, "groupName"); got != tc.wantParam {
				t.Errorf("groupName = %q, want %q", got, tc.wantParam)
			}
		})
	}
}

func TestCommandContentPrefersSnakeCaseParams(t *testing.T) {
	var cmd CommandContent
	raw := `{"request_id":"r","params":{"groupName":"from-snake"},"payload":{"groupName":"from-camel"},"requestId":"ignored"}`
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmd.RequestID != "r" {
		t.Errorf("RequestID = %q, the snake_case value wins", cmd.RequestID)
	}
	if got := getString(cmd.Params, "groupName"); got != "from-snake" {
		t.Errorf("groupName = %q, want the snake_case params", got)
	}
}

func TestCommandContentAlwaysGivesAMap(t *testing.T) {
	for _, raw := range []string{
		`{"request_id":"r","service":"ping","action":"ping"}`,
		`{"request_id":"r","service":"ping","action":"ping","params":null}`,
		`{"requestId":"r","service":"ping","action":"ping","payload":null}`,
	} {
		var cmd CommandContent
		if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if cmd.Params == nil {
			t.Fatalf("Params must never be nil for %s", raw)
		}
	}
}
