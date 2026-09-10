package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/eushing/agentwork/internal/service"
)

// ── templates (GitCode YAML 模板：列表 / 详情 / 刷新 / apply) ──

// listTemplates returns the cached template metadata. ?type=squad|project
// filters (full kind strings also accepted; ” = all).
func (h *Handlers) listTemplates(w http.ResponseWriter, r *http.Request) {
	if h.Templates == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("template service not configured"))
		return
	}
	out, err := h.Templates.List(r.Context(), r.URL.Query().Get("type"))
	writeJSON(w, out, err)
}

// getTemplate returns one template in full: metadata + raw YAML + JSON spec.
func (h *Handlers) getTemplate(w http.ResponseWriter, r *http.Request) {
	if h.Templates == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("template service not configured"))
		return
	}
	out, err := h.Templates.Get(r.Context(), r.PathValue("kind"), r.PathValue("id"))
	writeJSON(w, out, err)
}

// refreshTemplates force-refetches the template repo (body.token optional —
// private template repos need it; public repos pass nothing).
func (h *Handlers) refreshTemplates(w http.ResponseWriter, r *http.Request) {
	if h.Templates == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("template service not configured"))
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body = public repo
	out, err := h.Templates.Refresh(r.Context(), body.Token)
	writeJSON(w, out, err)
}

// applySquadTemplate applies a squad template: agents → squad → members.
func (h *Handlers) applySquadTemplate(w http.ResponseWriter, r *http.Request) {
	if h.TemplateApply == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("template apply service not configured"))
		return
	}
	var ov service.ApplySquadOverrides
	if err := json.NewDecoder(r.Body).Decode(&ov); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.TemplateApply.ApplySquad(r.Context(), r.PathValue("id"), ov)
	writeJSON(w, out, err)
}

// applyProjectTemplate applies a project template: repo → domain → team →
// goals → schedules.
func (h *Handlers) applyProjectTemplate(w http.ResponseWriter, r *http.Request) {
	if h.TemplateApply == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("template apply service not configured"))
		return
	}
	var ov service.ApplyProjectOverrides
	if err := json.NewDecoder(r.Body).Decode(&ov); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.TemplateApply.ApplyProject(r.Context(), r.PathValue("id"), ov)
	writeJSON(w, out, err)
}
