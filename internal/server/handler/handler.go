// Package handler is the HTTP boundary for task/agent/runtime CRUD.
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
	Task     *service.TaskService
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

	mux.HandleFunc("GET /tasks", h.listTasks)
	mux.HandleFunc("POST /tasks", h.createTask)
	mux.HandleFunc("GET /tasks/{id}", h.getTask)
	mux.HandleFunc("DELETE /tasks/{id}", h.deleteTask)
	mux.HandleFunc("POST /tasks/{id}/assign", h.assignTask)
	mux.HandleFunc("POST /tasks/{id}/cancel", h.cancelTask)
	mux.HandleFunc("POST /tasks/{id}/messages", h.postTaskMessage)
	mux.HandleFunc("POST /tasks/{id}/wait", h.waitTask)

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

// ── task ──

func (h *Handlers) createTask(w http.ResponseWriter, r *http.Request) {
	var t service.Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Task.Create(r.Context(), t)
	writeJSON(w, out, err)
}

func (h *Handlers) listTasks(w http.ResponseWriter, r *http.Request) {
	out, err := h.Task.List(r.Context())
	writeJSON(w, out, err)
}

func (h *Handlers) getTask(w http.ResponseWriter, r *http.Request) {
	out, err := h.Task.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

func (h *Handlers) assignTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AssigneeType string `json:"assignee_type"`
		AssigneeID   string `json:"assignee_id"`
		HandoffNote  string `json:"handoff_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Task.Assign(r.Context(), r.PathValue("id"), body.AssigneeType, body.AssigneeID, body.HandoffNote)
	writeJSON(w, out, err)
}

func (h *Handlers) postTaskMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := h.Task.AddMessage(r.Context(), r.PathValue("id"), body.Role, body.Text); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) waitTask(w http.ResponseWriter, r *http.Request) {
	if err := h.Task.WaitChildren(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) cancelTask(w http.ResponseWriter, r *http.Request) {
	out, err := h.Task.Cancel(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

func (h *Handlers) deleteTask(w http.ResponseWriter, r *http.Request) {
	if err := h.Task.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
