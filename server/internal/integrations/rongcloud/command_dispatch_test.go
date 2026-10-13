package rongcloud

import (
	"testing"
	"time"
)

func TestCommandDedupKeysUsesMessageUIDAndBothEnvelopeDialects(t *testing.T) {
	cases := []struct {
		name string
		msg  NormalizedMessage
		want []string
	}{
		{
			// 设备 CLI 发 snake_case 信封。
			name: "device snake_case",
			msg: NormalizedMessage{
				MsgUID:  "uid-1",
				Content: `{"request_id":"req-device","service":"discussion","action":"recommendRoles"}`,
			},
			want: []string{"uid:uid-1", "req:req-device"},
		},
		{
			// C 端 web 发 camelCase 信封。
			name: "web camelCase",
			msg: NormalizedMessage{
				MsgUID:  "uid-2",
				Content: `{"requestId":"req-web","service":"discussion","action":"recommendRoles"}`,
			},
			want: []string{"uid:uid-2", "req:req-web"},
		},
		{
			// 融云重投有时会重新包装消息、只剩 request_id 可认。
			name: "rewrapped delivery keeps request id only",
			msg: NormalizedMessage{
				Content: `{"request_id":"req-again","service":"discussion","action":"createRoundtable"}`,
			},
			want: []string{"req:req-again"},
		},
		{
			name: "unparsable content falls back to message uid",
			msg:  NormalizedMessage{MsgUID: "uid-3", Content: "not json"},
			want: []string{"uid:uid-3"},
		},
		{
			name: "nothing to dedup on",
			msg:  NormalizedMessage{},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandDedupKeys(tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("commandDedupKeys() = %v, want %v", got, tc.want)
			}
			for index := range got {
				if got[index] != tc.want[index] {
					t.Fatalf("commandDedupKeys() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestCommandDedupClaimRejectsRedelivery(t *testing.T) {
	var dedup commandDedup
	keys := commandDedupKeys(NormalizedMessage{
		MsgUID:  "uid-1",
		Content: `{"request_id":"req-1"}`,
	})

	if !dedup.claimCommand(keys...) {
		t.Fatal("first delivery must be accepted")
	}
	if dedup.claimCommand(keys...) {
		t.Fatal("redelivery of the same command must be rejected")
	}
	// 只有信封携带的 request id 相同（消息被重新包装）也必须判重。
	if dedup.claimCommand("req:req-1") {
		t.Fatal("redelivery matched only by request id must be rejected")
	}
	// 不同请求互不影响。
	if !dedup.claimCommand("uid-2", "req:req-2") {
		t.Fatal("a different command must be accepted")
	}
}

func TestCommandDedupClaimAllowsOnlyOnceWithNoKeys(t *testing.T) {
	// 拿不到任何标识时无法判重，只能每次都放行（宁可重跑也不能丢命令）。
	var dedup commandDedup
	if !dedup.claimCommand() {
		t.Fatal("a command without identifiers must not be dropped")
	}
	if !dedup.claimCommand() {
		t.Fatal("a command without identifiers must not be dropped on the next call either")
	}
}

func TestCommandDedupExpiresOldEntries(t *testing.T) {
	dedup := commandDedup{seen: map[string]time.Time{
		"uid-old": time.Now().Add(-commandDedupWindow - time.Minute),
	}}

	// 过期条目必须被当作新命令接受。
	if !dedup.claimCommand("uid-old") {
		t.Fatal("an entry older than the dedup window must be accepted again")
	}
}

func TestCommandDedupEvictsExpiredEntriesUnderPressure(t *testing.T) {
	dedup := commandDedup{seen: make(map[string]time.Time, commandDedupMaxKeys+1)}
	for index := 0; index < commandDedupMaxKeys; index++ {
		dedup.seen[string(rune(index))] = time.Now().Add(-commandDedupWindow - time.Minute)
	}

	if !dedup.claimCommand("uid-fresh") {
		t.Fatal("a fresh command must be accepted")
	}
	if len(dedup.seen) > commandDedupMaxKeys {
		t.Fatalf("dedup table grew to %d entries, want at most %d", len(dedup.seen), commandDedupMaxKeys)
	}
}
