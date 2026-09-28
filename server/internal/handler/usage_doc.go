package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/internal/util"
)

// usageDocSlugPattern restricts slugs to URL-safe lowercase tokens so docs
// can be addressed by slug without escaping.
var usageDocSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// UsageDocResponse is the JSON shape returned for a usage doc.
type UsageDocResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Content     string `json:"content"`
	SortOrder   int32  `json:"sort_order"`
	Status      string `json:"status"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func usageDocToResponse(d db.UsageDoc) UsageDocResponse {
	resp := UsageDocResponse{
		Slug:      d.Slug,
		Title:     d.Title,
		Category:  d.Category,
		Content:   d.Content,
		SortOrder: d.SortOrder,
		Status:    d.Status,
	}
	if d.ID.Valid {
		resp.ID = d.ID.String()
	}
	if d.WorkspaceID.Valid {
		resp.WorkspaceID = d.WorkspaceID.String()
	}
	if d.CreatedBy.Valid {
		resp.CreatedBy = d.CreatedBy.String()
	}
	if d.CreatedAt.Valid {
		resp.CreatedAt = d.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00")
	}
	if d.UpdatedAt.Valid {
		resp.UpdatedAt = d.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

type CreateUsageDocRequest struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Category  string `json:"category"`
	Content   string `json:"content"`
	SortOrder int32  `json:"sort_order"`
	Status    string `json:"status"`
}

type UpdateUsageDocRequest struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Category  string `json:"category"`
	Content   string `json:"content"`
	SortOrder int32  `json:"sort_order"`
	Status    string `json:"status"`
}

// validateUsageDoc normalizes and validates shared fields.
func validateUsageDoc(slug, title, category, status string, w http.ResponseWriter) (string, string, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !usageDocSlugPattern.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "slug must be lowercase letters, digits, and hyphens")
		return "", "", false
	}
	if strings.TrimSpace(title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return "", "", false
	}
	if category == "" {
		category = "general"
	}
	if status == "" {
		status = "draft"
	}
	if status != "draft" && status != "published" {
		writeError(w, http.StatusBadRequest, "status must be draft or published")
		return "", "", false
	}
	return category, status, true
}

// ListUsageDocs — GET /api/workspaces/{id}/usage-docs?include_drafts=1
func (h *Handler) ListUsageDocs(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}

	var docs []db.UsageDoc
	if r.URL.Query().Get("include_drafts") == "1" {
		member, ok := h.workspaceMember(w, r, workspaceID)
		if !ok {
			return
		}
		if member.Role != "owner" && member.Role != "admin" {
			writeError(w, http.StatusForbidden, "admin role required to list drafts")
			return
		}
		docs, err = h.Queries.ListUsageDocsByWorkspace(r.Context(), wsUUID)
	} else {
		docs, err = h.Queries.ListPublishedUsageDocsByWorkspace(r.Context(), wsUUID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list usage docs")
		return
	}

	resp := make([]UsageDocResponse, 0, len(docs))
	for _, d := range docs {
		resp = append(resp, usageDocToResponse(d))
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetUsageDoc — GET /api/workspaces/{id}/usage-docs/{slug}
func (h *Handler) GetUsageDoc(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}
	slug := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "slug")))

	doc, err := h.Queries.GetUsageDocBySlugAnyStatus(r.Context(), db.GetUsageDocBySlugAnyStatusParams{
		Slug:    slug,
		Column2: wsUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Fall back to the global doc scope (workspace_id IS NULL).
			doc, err = h.Queries.GetUsageDocBySlugAnyStatus(r.Context(), db.GetUsageDocBySlugAnyStatusParams{
				Slug:    slug,
				Column2: pgtype.UUID{},
			})
		}
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "usage doc not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to load usage doc")
			return
		}
	}

	if doc.Status != "published" {
		member, ok := h.workspaceMember(w, r, workspaceID)
		if !ok {
			return
		}
		if member.Role != "owner" && member.Role != "admin" {
			writeError(w, http.StatusNotFound, "usage doc not found")
			return
		}
	}

	writeJSON(w, http.StatusOK, usageDocToResponse(doc))
}

// CreateUsageDoc — POST /api/workspaces/{id}/usage-docs
func (h *Handler) CreateUsageDoc(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if member.Role != "owner" && member.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	var req CreateUsageDocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	category, status, ok := validateUsageDoc(req.Slug, req.Title, req.Category, req.Status, w)
	if !ok {
		return
	}

	doc, err := h.Queries.CreateUsageDoc(r.Context(), db.CreateUsageDocParams{
		WorkspaceID: wsUUID,
		Slug:        strings.ToLower(strings.TrimSpace(req.Slug)),
		Title:       strings.TrimSpace(req.Title),
		Category:    category,
		Content:     req.Content,
		SortOrder:   req.SortOrder,
		Status:      status,
		CreatedBy:   member.UserID,
	})
	if err != nil {
		if strings.Contains(err.Error(), "usage_doc_workspace_slug_idx") {
			writeError(w, http.StatusConflict, "a doc with this slug already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create usage doc")
		return
	}
	writeJSON(w, http.StatusCreated, usageDocToResponse(doc))
}

// UpdateUsageDoc — PUT /api/workspaces/{id}/usage-docs/{docId}
// Global docs (workspace_id NULL) are read-only templates.
func (h *Handler) UpdateUsageDoc(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	_, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if member.Role != "owner" && member.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	docID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "docId"), "doc id")
	if !ok {
		return
	}

	var req UpdateUsageDocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	category, status, ok := validateUsageDoc(req.Slug, req.Title, req.Category, req.Status, w)
	if !ok {
		return
	}

	existing, err := h.Queries.GetUsageDocByID(r.Context(), docID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "usage doc not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load usage doc")
		return
	}
	if !existing.WorkspaceID.Valid {
		writeError(w, http.StatusForbidden, "global docs are read-only")
		return
	}

	doc, err := h.Queries.UpdateUsageDoc(r.Context(), db.UpdateUsageDocParams{
		ID:        docID,
		Slug:      strings.ToLower(strings.TrimSpace(req.Slug)),
		Title:     strings.TrimSpace(req.Title),
		Category:  category,
		Content:   req.Content,
		SortOrder: req.SortOrder,
		Status:    status,
	})
	if err != nil {
		if strings.Contains(err.Error(), "usage_doc_workspace_slug_idx") {
			writeError(w, http.StatusConflict, "a doc with this slug already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update usage doc")
		return
	}
	writeJSON(w, http.StatusOK, usageDocToResponse(doc))
}

// DeleteUsageDoc — DELETE /api/workspaces/{id}/usage-docs/{docId}
func (h *Handler) DeleteUsageDoc(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	_, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if member.Role != "owner" && member.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	docID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "docId"), "doc id")
	if !ok {
		return
	}

	existing, err := h.Queries.GetUsageDocByID(r.Context(), docID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "usage doc not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load usage doc")
		return
	}
	if !existing.WorkspaceID.Valid {
		writeError(w, http.StatusForbidden, "global docs are read-only")
		return
	}

	rows, err := h.Queries.DeleteUsageDoc(r.Context(), docID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete usage doc")
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "usage doc not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
