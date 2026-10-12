package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// personaRecorder captures every private message the coordinator publishes.
type personaRecorder struct {
	mu       sync.Mutex
	messages []map[string]string
}

func newPersonaRecorder(t *testing.T) (*personaRecorder, *httptest.Server) {
	t.Helper()
	rec := &personaRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.URL.Path == "/message/private/publish.json" {
			msg := make(map[string]string, len(r.PostForm))
			for k, v := range r.PostForm {
				msg[k] = strings.Join(v, ",")
			}
			rec.mu.Lock()
			rec.messages = append(rec.messages, msg)
			rec.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"msgUID":"uid-1"}`)
	}))
	return rec, srv
}

func (r *personaRecorder) lastContent(t *testing.T) map[string]interface{} {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.messages) == 0 {
		t.Fatal("no private message was published")
	}
	raw := r.messages[len(r.messages)-1]["content"]
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("published content is not JSON: %v (%s)", err, raw)
	}
	return decoded
}

func personaUUID(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{b}, Valid: true}
}

// newPersonaCoordinator builds just enough coordinator to exercise the persona
// plumbing: a client pointed at the recorder, a state with one host and one
// ordinary speaker.
func newPersonaCoordinator(t *testing.T, state DiscussionState) (*DiscussionCoordinator, *personaRecorder) {
	t.Helper()
	rec, srv := newPersonaRecorder(t)
	t.Cleanup(srv.Close)
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	c := NewDiscussionCoordinator(personaUUID(9), personaUUID(8), state, client, nil, nil, nil, testLogger())
	return c, rec
}

func TestPersonaForPrefersHostPromptOnTheHostNode(t *testing.T) {
	hostID := personaUUID(1)
	memberID := personaUUID(2)

	cases := []struct {
		name    string
		state   DiscussionState
		speaker SpeakerInfo
		want    string
	}{
		{
			name:    "host with both prompts gets the operator text first",
			state:   DiscussionState{HostNodeID: hostID, HostPrompt: "你是主持人"},
			speaker: SpeakerInfo{NodeID: hostID, RolePrompt: "你是架构师"},
			want:    "你是主持人\n\n你是架构师",
		},
		{
			name:    "host with only the operator text",
			state:   DiscussionState{HostNodeID: hostID, HostPrompt: "你是主持人"},
			speaker: SpeakerInfo{NodeID: hostID},
			want:    "你是主持人",
		},
		{
			name:    "host with only a role prompt",
			state:   DiscussionState{HostNodeID: hostID},
			speaker: SpeakerInfo{NodeID: hostID, RolePrompt: "你是架构师"},
			want:    "你是架构师",
		},
		{
			// The operator text describes the moderator, so it must not leak
			// onto the other participants.
			name:    "non-host never receives the operator text",
			state:   DiscussionState{HostNodeID: hostID, HostPrompt: "你是主持人"},
			speaker: SpeakerInfo{NodeID: memberID, RolePrompt: "你是质疑者"},
			want:    "你是质疑者",
		},
		{
			name:    "non-host with no role prompt gets nothing",
			state:   DiscussionState{HostNodeID: hostID, HostPrompt: "你是主持人"},
			speaker: SpeakerInfo{NodeID: memberID},
			want:    "",
		},
		{
			name:    "no host configured and no prompts",
			state:   DiscussionState{},
			speaker: SpeakerInfo{NodeID: memberID},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &DiscussionCoordinator{state: tc.state}
			if got := c.personaFor(tc.speaker); got != tc.want {
				t.Errorf("personaFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSendYourTurnCarriesThePersona(t *testing.T) {
	hostID := personaUUID(1)
	state := DiscussionState{
		Status:     StatusInProgress,
		HostNodeID: hostID,
		HostPrompt: "你是主持人，负责收敛分歧。",
		Speakers: []SpeakerInfo{{
			NodeID:          hostID,
			RongcloudUserID: "rc_host",
			RoleName:        "主持人",
		}},
	}
	c, rec := newPersonaCoordinator(t, state)

	speaker := SpeakerInfo{
		NodeID:          hostID,
		RongcloudUserID: "rc_host",
		RoleName:        "主持人",
		Model:           "openai/gpt-5",
		RolePrompt:      "你负责开场与收尾。",
	}
	if err := c.sendYourTurn(context.Background(), speaker, 2, 1); err != nil {
		t.Fatalf("sendYourTurn: %v", err)
	}

	content := rec.lastContent(t)
	if content["service"] != "discussion" || content["action"] != "your_turn" {
		t.Errorf("envelope = %v, want service=discussion action=your_turn", content)
	}
	params, ok := content["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("params missing or not an object: %#v", content["params"])
	}
	// The host carries both blocks, operator text first.
	if got := params["role_prompt"]; got != "你是主持人，负责收敛分歧。\n\n你负责开场与收尾。" {
		t.Errorf("role_prompt = %q", got)
	}
	// The pre-existing fields must survive.
	if params["role_name"] != "主持人" || params["model"] != "openai/gpt-5" {
		t.Errorf("role_name/model regressed: %#v", params)
	}
	if params["chatroom_id"] != chatroomKey(personaUUID(9)) {
		t.Errorf("chatroom_id = %v", params["chatroom_id"])
	}
	if _, ok := params["round"].(float64); !ok {
		t.Errorf("round missing: %#v", params["round"])
	}
}

func TestSendYourTurnOmitsBlankPersona(t *testing.T) {
	hostID := personaUUID(1)
	state := DiscussionState{
		Status:     StatusInProgress,
		HostNodeID: hostID,
		Speakers:   []SpeakerInfo{{NodeID: hostID, RongcloudUserID: "rc_host"}},
	}
	c, rec := newPersonaCoordinator(t, state)

	// A deployment with nothing configured must still produce a clean payload —
	// the key is absent rather than an empty string, so an older reader sees
	// exactly the shape it always did.
	if err := c.sendYourTurn(context.Background(), state.Speakers[0], 1, 1); err != nil {
		t.Fatalf("sendYourTurn: %v", err)
	}
	params, ok := rec.lastContent(t)["params"].(map[string]interface{})
	if !ok {
		t.Fatal("params missing")
	}
	if _, present := params["role_prompt"]; present {
		t.Errorf("role_prompt should be omitted when blank, got %#v", params["role_prompt"])
	}
}

func TestBuildPromptIncludesThePersona(t *testing.T) {
	hostID := personaUUID(1)
	c := &DiscussionCoordinator{
		state: DiscussionState{HostNodeID: hostID, HostPrompt: "你是主持人。"},
	}
	speaker := SpeakerInfo{NodeID: hostID, RoleName: "主持人", RolePrompt: "你负责控场。"}

	prompt := c.buildPrompt(speaker, 2, 1)
	for _, want := range []string{"round 2", "Your role: 主持人", "你是主持人。", "你负责控场。", "Speaking order: 1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("buildPrompt() missing %q:\n%s", want, prompt)
		}
	}

	// Without a persona the prompt must look exactly like it used to.
	plain := (&DiscussionCoordinator{}).buildPrompt(SpeakerInfo{RoleName: "观察者"}, 1, 1)
	if strings.Contains(plain, "你是主持人") {
		t.Errorf("persona leaked into a coordinator with none:\n%s", plain)
	}
	if !strings.Contains(plain, "Your role: 观察者") {
		t.Errorf("plain prompt lost its role line:\n%s", plain)
	}
}

func TestHostPromptLoaderDegradesQuietly(t *testing.T) {
	// nil loader: the service must not panic and must report "no persona".
	if got := (&DiscussionService{}).hostPrompt(context.Background()); got != "" {
		t.Errorf("nil loader = %q, want empty", got)
	}
	// Whitespace-only config counts as unset, otherwise a stray newline from
	// the admin textarea would inject a blank persona block.
	svc := &DiscussionService{hostPersona: func(context.Context) string { return "  \n\t " }}
	if got := svc.hostPrompt(context.Background()); got != "" {
		t.Errorf("blank loader = %q, want empty", got)
	}
	svc = &DiscussionService{hostPersona: func(context.Context) string { return " 你是主持人 " }}
	if got := svc.hostPrompt(context.Background()); got != "你是主持人" {
		t.Errorf("loader = %q, want the trimmed text", got)
	}
}
