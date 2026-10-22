package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func newOnboardingPromptHandler() *Handler {
	return &Handler{DB: testPool}
}

// adminTestToken seeds one admin account with system:write and mints a real
// access JWT, so the test exercises the same requireClawAdmin path the admin
// web app goes through (JWT decode + permission check), not a bypass.
func adminTestToken(t *testing.T) string {
	t.Helper()
	suffix := uuid.NewString()[:8]
	roleID := "role_" + suffix
	adminID := "adm_" + suffix
	dbfx.InsertNoID(t, "claw_admin_roles", testutil.Cols{
		"role_id":     roleID,
		"name":        "onboarding-test-" + suffix,
		"role_name":   "接入提示词测试",
		"status":      "active",
		"permissions": testutil.Raw(`ARRAY['system:read','system:write']`),
	}, "role_id = $1", roleID)
	dbfx.InsertNoID(t, "claw_admin_users", testutil.Cols{
		"admin_id":      adminID,
		"username":      "onboarding_" + suffix,
		"nickname":      "onboarding test",
		"password_hash": "x",
		"role_id":       roleID,
		"status":        "active",
	}, "admin_id = $1", adminID)
	token, err := issueClawAdminJWT(adminID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue admin jwt: %v", err)
	}
	return token
}

// clearOnboardingPrompt removes the config row so a test starts from the
// "not configured" state the first deploy sees.
func clearOnboardingPrompt(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, "DELETE FROM rongcloud_system_config WHERE config_key = $1", onboardingPromptConfigKey); err != nil {
		t.Fatalf("clear onboarding prompt: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM rongcloud_system_config WHERE config_key = $1", onboardingPromptConfigKey)
	})
}

func TestClawOnboardingPromptFallsBackWhenUnset(t *testing.T) {
	clearOnboardingPrompt(t)
	h := newOnboardingPromptHandler()

	resp := testutil.Call(t, h.ClawOnboardingPrompt,
		testutil.JSONRequest(http.MethodGet, "/api/claw/onboarding-prompt", nil),
	).Want(http.StatusOK).Body

	var payload struct {
		Data struct {
			Text   string `json:"text"`
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.String())
	}
	// No row means "use the site's bundled copy", so text stays empty and the
	// client is told why — not a 404, which would look like a broken deploy.
	if payload.Data.Text != "" || payload.Data.Source != "default" {
		t.Fatalf("unset prompt = %+v, want empty text and source=default", payload.Data)
	}
}

func TestClawAdminOnboardingPromptRoundTrip(t *testing.T) {
	clearOnboardingPrompt(t)
	h := newOnboardingPromptHandler()
	token := adminTestToken(t)

	// Write through the admin endpoint (plain text in, plain text out). The
	// stored text is whitespace-trimmed at the edges, so the fixture has no
	// trailing newline; internal blank lines must survive byte for byte.
	body := "你是我的设备接入执行员。\n\n═══ B. 增量接入 ═══\n· 不需要任何票据。\n   缩进也要保留"
	testutil.Call(t, h.ClawAdminOnboardingPromptPut,
		testutil.WithHeaders(
			testutil.JSONRequest(http.MethodPut, "/api/admin/onboarding-prompt", map[string]any{"text": body}),
			"Authorization", "Bearer "+token,
		),
	).Want(http.StatusOK)

	// The public endpoint serves exactly that text.
	resp := testutil.Call(t, h.ClawOnboardingPrompt,
		testutil.JSONRequest(http.MethodGet, "/api/claw/onboarding-prompt", nil),
	).Want(http.StatusOK).Body
	var payload struct {
		Data struct {
			Text      string `json:"text"`
			Source    string `json:"source"`
			UpdatedAt string `json:"updated_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.String())
	}
	if payload.Data.Text != body {
		t.Fatalf("served text = %q, want %q", payload.Data.Text, body)
	}
	if !strings.Contains(payload.Data.Text, "\n\n═══ B. 增量接入 ═══\n") {
		t.Fatalf("internal newlines were mangled: %q", payload.Data.Text)
	}
	if payload.Data.Source != "database" {
		t.Fatalf("source = %q, want database", payload.Data.Source)
	}
	if payload.Data.UpdatedAt == "" {
		t.Fatal("updated_at should be populated for a stored prompt")
	}
}

func TestClawAdminOnboardingPromptClearsToDefault(t *testing.T) {
	clearOnboardingPrompt(t)
	h := newOnboardingPromptHandler()
	token := adminTestToken(t)

	testutil.Call(t, h.ClawAdminOnboardingPromptPut,
		testutil.WithHeaders(
			testutil.JSONRequest(http.MethodPut, "/api/admin/onboarding-prompt", map[string]any{"text": "临时文案"}),
			"Authorization", "Bearer "+token,
		),
	).Want(http.StatusOK)

	// An empty text is the documented rollback: the site falls back to the
	// bundled copy instead of rendering a blank appendix.
	testutil.Call(t, h.ClawAdminOnboardingPromptPut,
		testutil.WithHeaders(
			testutil.JSONRequest(http.MethodPut, "/api/admin/onboarding-prompt", map[string]any{"text": "   "}),
			"Authorization", "Bearer "+token,
		),
	).Want(http.StatusOK)

	resp := testutil.Call(t, h.ClawOnboardingPrompt,
		testutil.JSONRequest(http.MethodGet, "/api/claw/onboarding-prompt", nil),
	).Want(http.StatusOK).Body
	var payload struct {
		Data struct {
			Text   string `json:"text"`
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Data.Text != "" || payload.Data.Source != "default" {
		t.Fatalf("after clearing = %+v, want empty text and source=default", payload.Data)
	}
}

func TestClawAdminOnboardingPromptRequiresAdmin(t *testing.T) {
	h := newOnboardingPromptHandler()
	// No bearer token: the admin surface must not be writable anonymously.
	testutil.Call(t, h.ClawAdminOnboardingPromptPut,
		testutil.JSONRequest(http.MethodPut, "/api/admin/onboarding-prompt", map[string]any{"text": "x"}),
	).Want(http.StatusUnauthorized)
	testutil.Call(t, h.ClawAdminOnboardingPromptGet,
		testutil.JSONRequest(http.MethodGet, "/api/admin/onboarding-prompt", nil),
	).Want(http.StatusUnauthorized)
}

func TestDecodePromptConfigShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"envelope from the admin endpoint", `{"value":"hello"}`, "hello"},
		{"bare json string", `"hello"`, "hello"},
		{"empty object", `{}`, ""},
		{"null", `null`, ""},
		{"number", `3`, ""},
		{"empty bytes", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodePromptConfig([]byte(tc.raw)); got != tc.want {
				t.Fatalf("decodePromptConfig(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
