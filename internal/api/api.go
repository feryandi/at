package api

import (
	"encoding/json"
	"log"
	"net/http"
	"runtime/debug"

	"github.com/ferya/at/internal/deploy"
	"github.com/ferya/at/internal/proxy"
	"github.com/ferya/at/internal/store"
)

type Handler struct {
	db         *store.DB
	pipeline   *deploy.Pipeline
	caddy      *proxy.Caddy
	baseDomain string
}

func NewHandler(db *store.DB, pipeline *deploy.Pipeline, caddy *proxy.Caddy, baseDomain string) *Handler {
	return &Handler{db: db, pipeline: pipeline, caddy: caddy, baseDomain: baseDomain}
}

type AppWithStatus struct {
	*store.App
	LatestDeployment *store.Deployment `json:"latest_deployment,omitempty"`
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", h.handleDashboard)
	mux.HandleFunc("GET /api/config", h.handleConfig)
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/apps", h.handleListApps)
	mux.HandleFunc("POST /api/apps/scan", h.handleScanProjects)
	mux.HandleFunc("GET /api/apps/{id}", h.handleGetApp)
	mux.HandleFunc("PUT /api/apps/{id}", h.handleUpdateApp)
	mux.HandleFunc("DELETE /api/apps/{id}", h.handleDeleteApp)
	mux.HandleFunc("GET /api/apps/{id}/deployments", h.handleListDeployments)
	mux.HandleFunc("POST /api/apps/{id}/deploy", h.handleTriggerDeploy)
	mux.HandleFunc("GET /api/deployments/{id}", h.handleGetDeployment)
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	content, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(content); err != nil {
		log.Printf("[dashboard] write error: %v", err)
	}
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{
		"base_domain": h.baseDomain,
	})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{
		"caddy": h.caddy.Ping(r.Context()),
	})
}

func (h *Handler) handleListApps(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	apps, err := h.db.ListApps()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to list apps: "+err.Error())
		return
	}

	appIDs := make([]string, len(apps))
	for i, app := range apps {
		appIDs[i] = app.ID
	}
	latestDeps, err := h.db.GetLatestDeploymentsByAppIDs(appIDs)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to get deployments: "+err.Error())
		return
	}

	result := make([]AppWithStatus, 0, len(apps))
	for _, app := range apps {
		result = append(result, AppWithStatus{App: app, LatestDeployment: latestDeps[app.ID]})
	}

	jsonOK(w, result)
}

func (h *Handler) handleScanProjects(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	if err := h.pipeline.ScanProjects(); err != nil {
		jsonErr(w, http.StatusInternalServerError, "scan failed: "+err.Error())
		return
	}

	apps, err := h.db.ListApps()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to list apps: "+err.Error())
		return
	}

	jsonOK(w, map[string]any{"scanned": true, "total_apps": len(apps)})
}

func (h *Handler) handleGetApp(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")
	app, err := h.db.GetApp(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		jsonErr(w, http.StatusNotFound, "app not found")
		return
	}
	jsonOK(w, app)
}

func (h *Handler) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")
	app, err := h.db.GetApp(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		jsonErr(w, http.StatusNotFound, "app not found")
		return
	}

	var input struct {
		Domain        *string `json:"domain"`
		ContainerPort *int    `json:"container_port"`
		EnvVars       *string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if input.Domain != nil {
		app.Domain = *input.Domain
	}
	if input.ContainerPort != nil {
		app.ContainerPort = *input.ContainerPort
	}
	if input.EnvVars != nil {
		var check map[string]string
		if err := json.Unmarshal([]byte(*input.EnvVars), &check); err != nil {
			jsonErr(w, http.StatusBadRequest, "env_vars must be a valid JSON object: "+err.Error())
			return
		}
		app.EnvVars = *input.EnvVars
	}

	if err := h.db.UpdateApp(app); err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to update app: "+err.Error())
		return
	}
	jsonOK(w, app)
}

func (h *Handler) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")
	app, err := h.db.GetApp(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		jsonErr(w, http.StatusNotFound, "app not found")
		return
	}

	h.pipeline.StopAppContainer(r.Context(), app)

	if err := h.db.DeleteApp(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to delete app: "+err.Error())
		return
	}

	if err := h.pipeline.SyncCaddyPublic(r.Context()); err != nil {
		log.Printf("[caddy] sync after delete app %s: %v", id, err)
	}

	jsonOK(w, map[string]string{"status": "deleted"})
}

func (h *Handler) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")

	app, err := h.db.GetApp(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		jsonErr(w, http.StatusNotFound, "app not found")
		return
	}

	deps, err := h.db.ListDeployments(id, 20)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if deps == nil {
		deps = []*store.Deployment{}
	}
	jsonOK(w, deps)
}

func (h *Handler) handleTriggerDeploy(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")

	app, err := h.db.GetApp(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		jsonErr(w, http.StatusNotFound, "app not found")
		return
	}

	depID, err := h.pipeline.Deploy(id)
	if err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}

	jsonOK(w, map[string]string{"deployment_id": depID})
}

func (h *Handler) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w)

	id := r.PathValue("id")
	dep, err := h.db.GetDeployment(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dep == nil {
		jsonErr(w, http.StatusNotFound, "deployment not found")
		return
	}
	jsonOK(w, dep)
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func recoverHandler(w http.ResponseWriter) {
	if r := recover(); r != nil {
		log.Printf("[panic] handler panic: %v\n%s", r, debug.Stack())
		jsonErr(w, http.StatusInternalServerError, "internal server error")
	}
}
