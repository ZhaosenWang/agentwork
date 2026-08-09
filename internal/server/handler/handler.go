// Package handler is the HTTP boundary for goal/run/agent/runtime/squad/
// comment/schedule CRUD.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/eushing/agentwork/internal/service"
)

type Handlers struct {
	Runtime  *service.RuntimeService
	Agent    *service.AgentService
	Goal     *service.GoalService
	Run      *service.RunService
	Comment  *service.CommentService
	Squad    *service.SquadService
	Schedule *service.ScheduleService
}

func (h *Handlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /runtimes", h.listRuntimes)
	mux.HandleFunc("POST /runtimes", h.createRuntime)
	mux.HandleFunc("GET /runtimes/{id}", h.getRuntime)
	mux.HandleFunc("DELETE /runtimes/{id}", h.deleteRuntime)

	mux.HandleFunc("GET /agents", h.listAgents)
	mux.HandleFunc("POST /agents", h.createAgent)
	mux.HandleFunc("GET /agents/{id}", h.getAgent)
	mux.HandleFunc("DELETE /agents/{id}", h.deleteAgent)

	mux.HandleFunc("GET /goals", h.listGoals)
	mux.HandleFunc("POST /goals", h.createGoal)
	mux.HandleFunc("GET /goals/{id}", h.getGoal)
	mux.HandleFunc("DELETE /goals/{id}", h.deleteGoal)
	mux.HandleFunc("POST /goals/{id}/assign", h.assignGoal)
	mux.HandleFunc("POST /goals/{id}/cancel", h.cancelGoal)
	mux.HandleFunc("POST /goals/{id}/done", h.doneGoal)
	mux.HandleFunc("POST /goals/{id}/fail", h.failGoal)
	mux.HandleFunc("GET /goals/{id}/runs", h.listRuns)
	mux.HandleFunc("GET /goals/{id}/comments", h.listComments)
	mux.HandleFunc("POST /goals/{id}/comments", h.createComment)

	mux.HandleFunc("GET /squads", h.listSquads)
	mux.HandleFunc("POST /squads", h.createSquad)
	mux.HandleFunc("GET /squads/{id}", h.getSquad)
	mux.HandleFunc("DELETE /squads/{id}", h.deleteSquad)
	mux.HandleFunc("POST /squads/{id}/members", h.addSquadMember)
	mux.HandleFunc("GET /squads/{id}/members", h.listSquadMembers)

	mux.HandleFunc("GET /schedules", h.listSchedules)
	mux.HandleFunc("POST /schedules", h.createSchedule)
	mux.HandleFunc("GET /schedules/{id}", h.getSchedule)
	mux.HandleFunc("DELETE /schedules/{id}", h.deleteSchedule)
}

// ── runtime ──

func (h *Handlers) createRuntime(w http.ResponseWriter, r *http.Request) {
	var rt service.Runtime
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Runtime.Create(r.Context(), rt)
	writeJSON(w, out, err)
}
func (h *Handlers) listRuntimes(w http.ResponseWriter, r *http.Request) {
	out, err := h.Runtime.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getRuntime(w http.ResponseWriter, r *http.Request) {
	out, err := h.Runtime.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteRuntime(w http.ResponseWriter, r *http.Request) {
	if err := h.Runtime.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── agent ──

func (h *Handlers) createAgent(w http.ResponseWriter, r *http.Request) {
	var a service.Agent
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Agent.Create(r.Context(), a)
	writeJSON(w, out, err)
}
func (h *Handlers) listAgents(w http.ResponseWriter, r *http.Request) {
	out, err := h.Agent.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getAgent(w http.ResponseWriter, r *http.Request) {
	out, err := h.Agent.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.Agent.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── goal ──

func (h *Handlers) createGoal(w http.ResponseWriter, r *http.Request) {
	var g service.Goal
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Goal.Create(r.Context(), g)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	// A freshly-created active goal with an agent/squad assignee needs a first
	// run enqueued (backlog goals do NOT — the semantic invariant).
	if out.Status == "active" && (out.AssigneeType == "agent" || out.AssigneeType == "squad") {
		_, _ = h.Run.EnqueueForGoal(r.Context(), *out)
	}
	writeJSON(w, out, nil)
}
func (h *Handlers) listGoals(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getGoal(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteGoal(w http.ResponseWriter, r *http.Request) {
	if err := h.Goal.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) assignGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AssigneeType string `json:"assignee_type"`
		AssigneeID   string `json:"assignee_id"`
		HandoffNote  string `json:"handoff_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Goal.Assign(r.Context(), r.PathValue("id"), body.AssigneeType, body.AssigneeID, body.HandoffNote)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	// Enqueue a run for the new assignee (coalesces if one is pending). The
	// prior run, if in flight, keeps running — reconcile discards its result.
	if body.AssigneeType == "agent" || body.AssigneeType == "squad" {
		_, _ = h.Run.EnqueueForGoal(r.Context(), *out)
	}
	writeJSON(w, out, nil)
}
func (h *Handlers) cancelGoal(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Cancel(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) doneGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agent_id"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := h.Goal.MarkDone(r.Context(), r.PathValue("id"), body.AgentID, body.Summary); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) failGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agent_id"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := h.Goal.MarkFailed(r.Context(), r.PathValue("id"), body.AgentID, body.Summary); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) listRuns(w http.ResponseWriter, r *http.Request) {
	out, err := h.Run.List(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// ── comment ──

func (h *Handlers) createComment(w http.ResponseWriter, r *http.Request) {
	var c service.Comment
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c.GoalID = r.PathValue("id")
	out, err := h.Comment.Create(r.Context(), c)
	writeJSON(w, out, err)
}
func (h *Handlers) listComments(w http.ResponseWriter, r *http.Request) {
	out, err := h.Comment.List(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// ── squad ──

func (h *Handlers) createSquad(w http.ResponseWriter, r *http.Request) {
	var sq service.Squad
	if err := json.NewDecoder(r.Body).Decode(&sq); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Squad.Create(r.Context(), sq)
	writeJSON(w, out, err)
}
func (h *Handlers) listSquads(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getSquad(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteSquad(w http.ResponseWriter, r *http.Request) {
	if err := h.Squad.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) addSquadMember(w http.ResponseWriter, r *http.Request) {
	var m service.SquadMember
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Squad.AddMember(r.Context(), r.PathValue("id"), m.MemberType, m.MemberID, m.Role)
	writeJSON(w, out, err)
}
func (h *Handlers) listSquadMembers(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.ListMembers(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// ── schedule ──

func (h *Handlers) createSchedule(w http.ResponseWriter, r *http.Request) {
	var sch service.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Schedule.Create(r.Context(), sch)
	writeJSON(w, out, err)
}
func (h *Handlers) listSchedules(w http.ResponseWriter, r *http.Request) {
	out, err := h.Schedule.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getSchedule(w http.ResponseWriter, r *http.Request) {
	out, err := h.Schedule.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := h.Schedule.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──

// writeJSON writes v as JSON, or an error response if err is non-nil. err is
// mapped: ErrNotFound→404, ErrValidation→400, else 500.
func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, service.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, service.ErrValidation) {
			code = http.StatusBadRequest
		}
		writeErr(w, code, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}