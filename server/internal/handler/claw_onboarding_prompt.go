package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// onboarding-prompt is the "接入提示词" body shown in the guide appendix.
//
// It lives in the database rather than in the web bundle so editing the copy
// does not require rebuilding and re-uploading the SPA: the admin page writes
// this key, the public endpoint serves it, and the site falls back to its
// bundled copy when the key is unset (first deploy, or a bad edit rolled
// back). Stored in rongcloud_system_config as {"value": "<text>"} — the same
// envelope the generic admin config PUT produces for non-JSON values.
const onboardingPromptConfigKey = "agent_onboarding_prompt"

// maxOnboardingPromptBytes bounds the stored document. The current prompt is
// ~8 KB; 256 KB leaves room for years of copy edits while keeping a bad paste
// from filling the row.
const maxOnboardingPromptBytes = 256 << 10

type onboardingPromptView struct {
	Text      string `json:"text"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Source tells the client whether the text came from the database or is
	// simply absent (the site then uses its bundled copy).
	Source string `json:"source"`
}

// readOnboardingPrompt loads the stored prompt. A missing row is not an
// error: it means "not configured yet", which the caller renders as an empty
// text plus source=default.
func (h *Handler) readOnboardingPrompt(r *http.Request) (onboardingPromptView, error) {
	var raw []byte
	var updatedAt time.Time
	err := h.DB.QueryRow(r.Context(), `
		SELECT config, updated_at
		FROM rongcloud_system_config
		WHERE config_key = $1
		ORDER BY updated_at DESC
		LIMIT 1`, onboardingPromptConfigKey).Scan(&raw, &updatedAt)
	if err != nil {
		if isNotFound(err) {
			return onboardingPromptView{Source: "default"}, nil
		}
		return onboardingPromptView{}, err
	}
	text := decodePromptConfig(raw)
	view := onboardingPromptView{Text: text, Source: "database"}
	if !updatedAt.IsZero() {
		view.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	}
	if text == "" {
		view.Source = "default"
	}
	return view, nil
}

// decodePromptConfig accepts both shapes the config table can hold: the
// {"value": "..."} envelope written by the admin endpoint, and a bare JSON
// string (what a hand-written row or an older tool might produce).
func decodePromptConfig(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var envelope struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Value != "" {
		return envelope.Value
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	// Not JSON at all (a jsonb column cannot normally hold this, but a null or
	// a number would land here): treat as unset.
	return ""
}

// ClawOnboardingPrompt GET /api/claw/onboarding-prompt — public.
// The product site reads this on the guide page; no auth because the text is
// shown to every visitor anyway.
func (h *Handler) ClawOnboardingPrompt(w http.ResponseWriter, r *http.Request) {
	view, err := h.readOnboardingPrompt(r)
	if err != nil {
		slog.Warn("claw: read onboarding prompt failed", "error", err)
		clawJSON(w, 500, 500, "读取接入提示词失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", view)
}

// ClawAdminOnboardingPromptGet GET /api/admin/onboarding-prompt — admin read.
// Same payload as the public endpoint so the editor shows exactly what the
// site shows, including the "not configured" state.
func (h *Handler) ClawAdminOnboardingPromptGet(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:read", w, r) == nil {
		return
	}
	view, err := h.readOnboardingPrompt(r)
	if err != nil {
		slog.Warn("claw admin: read onboarding prompt failed", "error", err)
		clawAdminFail(w, 500, "database error")
		return
	}
	clawAdminOK(w, view)
}

// ClawAdminOnboardingPromptPut PUT /api/admin/onboarding-prompt — admin write.
// Empty text clears the override, which makes the site fall back to its
// bundled copy: that is the safe rollback when an edit reads badly.
func (h *Handler) ClawAdminOnboardingPromptPut(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxOnboardingPromptBytes+1024)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object with a text field is required")
		return
	}
	if len(body.Text) > maxOnboardingPromptBytes {
		clawAdminFail(w, 400, "prompt is too large")
		return
	}
	text := strings.TrimSpace(body.Text)

	// Upsert keyed by (workspace_id, config_key): reuse the workspace that
	// already holds this key, else the oldest workspace — the same resolution
	// the generic config PUT uses, so both paths edit the same row.
	var wsID string
	if err := h.DB.QueryRow(r.Context(),
		"SELECT workspace_id::text FROM rongcloud_system_config WHERE config_key=$1 LIMIT 1",
		onboardingPromptConfigKey).Scan(&wsID); err != nil {
		if err := h.DB.QueryRow(r.Context(),
			"SELECT id::text FROM workspace ORDER BY created_at LIMIT 1").Scan(&wsID); err != nil {
			clawAdminFail(w, 500, "no workspace available")
			return
		}
	}
	payload, err := json.Marshal(map[string]string{"value": text})
	if err != nil {
		clawAdminFail(w, 500, "encode prompt failed")
		return
	}
	// Same column set and conflict target as ClawAdminSystemConfigPut: both
	// paths must write the identical row, or the generic editor would show a
	// stale second copy.
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, config)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, config_key)
		DO UPDATE SET config = $3, config_version = rongcloud_system_config.config_version + 1, updated_at = now()`,
		wsID, onboardingPromptConfigKey, payload); err != nil {
		slog.Warn("claw admin: write onboarding prompt failed", "error", err)
		clawAdminFail(w, 500, "database error")
		return
	}
	h.clawAdminAudit(r, ident, "admin_onboarding_prompt_update", "system_config", onboardingPromptConfigKey,
		map[string]interface{}{"bytes": len(text)})
	clawAdminOK(w, onboardingPromptView{Text: text, Source: sourceFor(text), UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
}

func sourceFor(text string) string {
	if text == "" {
		return "default"
	}
	return "database"
}
