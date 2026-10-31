package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/backups"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/gateway"
	"vps-tool/control-plane/internal/models"
	"vps-tool/control-plane/internal/nodes"
	"vps-tool/control-plane/internal/scheduler"
)

type Server struct {
	DB      *db.DB
	Cfg     config.Settings
	Nodes   *nodes.Service
	Actions *actions.Service
	Gateway *gateway.Gateway
	Backups *backups.Service
	Tasks   *scheduler.TaskService
	Static  http.Handler
	Limiter *auth.Limiter
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/health" {
		s.health(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	if s.Static != nil {
		s.Static.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func errw(w http.ResponseWriter, status int, code, msg string) {
	write(w, status, map[string]any{"detail": map[string]string{"code": code, "message": msg}})
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (s *Server) health(w http.ResponseWriter) {
	var ok int
	e := s.DB.SQL.QueryRow("SELECT 1").Scan(&ok)
	service, status := "ok", http.StatusOK
	if e != nil || ok != 1 {
		service, status = "error", http.StatusServiceUnavailable
	}
	write(w, status, map[string]any{"ok": service == "ok", "service": service, "schema_version": db.SchemaVersion, "online_agents": len(s.Gateway.OnlineIDs()), "timestamp": time.Now().UTC().Format(time.RFC3339)})
}
func (s *Server) session(w http.ResponseWriter, r *http.Request, csrf bool) (auth.Session, bool) {
	var code int
	var msg string
	var x auth.Session
	if csrf {
		x, code, msg = auth.RequireCSRF(s.DB, s.Cfg, r)
	} else {
		x, code, msg = auth.Require(s.DB, s.Cfg, r)
	}
	if code != 0 {
		errw(w, code, msg, msg)
		return x, false
	}
	return x, true
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/")
	if p == "auth/login" && r.Method == http.MethodPost {
		s.login(w, r)
		return
	}
	if p == "auth/me" && r.Method == http.MethodGet {
		ss, ok := s.session(w, r, false)
		if ok {
			csrf, _ := auth.RotateCSRF(s.DB, ss)
			write(w, 200, map[string]any{"id": ss.UserID, "username": ss.Username, "csrf_token": csrf, "expires_at": ss.ExpiresAt})
		}
		return
	}
	if p == "auth/logout" && r.Method == http.MethodPost {
		ss, ok := s.session(w, r, true)
		if ok {
			if err := auth.Logout(s.DB, ss); err != nil {
				errw(w, 500, "logout_failed", "could not end session")
				return
			}
			auth.ClearCookie(w, s.Cfg)
			write(w, 200, map[string]string{"status": "ok"})
		}
		return
	}
	if p == "auth/change-password" && r.Method == http.MethodPost {
		ss, ok := s.session(w, r, true)
		if !ok {
			return
		}
		var b struct {
			Current string `json:"current_password"`
			Next    string `json:"new_password"`
			Confirm string `json:"confirm_password"`
		}
		if decode(r, &b) != nil || b.Next != b.Confirm {
			errw(w, 422, "password_confirmation_mismatch", "password confirmation does not match")
			return
		}
		if e := auth.ChangePassword(s.DB, ss, b.Current, b.Next); e != nil {
			status := 422
			if e.Error() == "current_password_invalid" {
				status = 403
			}
			errw(w, status, e.Error(), e.Error())
			return
		}
		auth.ClearCookie(w, s.Cfg)
		write(w, 200, map[string]string{"status": "password_changed"})
		return
	}
	if p == "nodes" && r.Method == http.MethodGet {
		s.listNodes(w, r)
		return
	}
	if p == "nodes" && r.Method == http.MethodPost {
		s.createNode(w, r)
		return
	}
	if p == "actions" && r.Method == http.MethodGet {
		s.listActions(w, r)
		return
	}
	if p == "action-requests" && r.Method == http.MethodGet {
		s.listRequests(w, r)
		return
	}
	if p == "tasks" && r.Method == http.MethodGet {
		s.listTasks(w, r)
		return
	}
	if p == "tasks" && r.Method == http.MethodPost {
		s.createTask(w, r)
		return
	}
	if strings.HasPrefix(p, "tasks/") {
		s.taskAPI(w, r, p)
		return
	}
	if strings.HasPrefix(p, "nodes/") {
		s.nodeAPI(w, r, p)
		return
	}
	if strings.HasPrefix(p, "action-requests/") {
		ss, ok := s.session(w, r, false)
		_ = ss
		if !ok {
			return
		}
		id := path.Base(p)
		m, e := s.Actions.Get(id)
		if e != nil {
			errw(w, 404, "request_not_found", "action request was not found")
		} else {
			write(w, 200, map[string]any{"request": m})
		}
		return
	}
	errw(w, 404, "not_found", "not found")
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r, false); !ok {
		return
	}
	write(w, 200, map[string]any{"tasks": s.Tasks.List()})
}
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r, true)
	if !ok {
		return
	}
	var b struct {
		Name          string         `json:"name"`
		NodeIDs       []string       `json:"node_ids"`
		Action        string         `json:"action"`
		Parameters    map[string]any `json:"parameters"`
		ScheduleType  string         `json:"schedule_type"`
		ScheduleValue string         `json:"schedule_value"`
		Timezone      string         `json:"timezone"`
		Enabled       *bool          `json:"enabled"`
		MaxRetries    *int           `json:"max_retries"`
		Retry         []int          `json:"retry_intervals_seconds"`
	}
	if e := decode(r, &b); e != nil {
		errw(w, 422, "invalid_parameters", "invalid JSON")
		return
	}
	if b.Timezone == "" {
		b.Timezone = "Asia/Shanghai"
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	maxRetries := 2
	if b.MaxRetries != nil {
		maxRetries = *b.MaxRetries
	}
	if b.Retry == nil {
		b.Retry = []int{30, 90}
	}
	m, e := s.Tasks.Create(b.Name, b.NodeIDs, b.Action, b.Parameters, b.ScheduleType, b.ScheduleValue, b.Timezone, enabled, maxRetries, b.Retry)
	if e != nil {
		errw(w, 422, "invalid_schedule", e.Error())
		return
	}
	_ = ss
	write(w, 200, map[string]any{"task": m})
}
func (s *Server) taskAPI(w http.ResponseWriter, r *http.Request, p string) {
	parts := strings.Split(strings.TrimPrefix(p, "tasks/"), "/")
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		if _, ok := s.session(w, r, false); !ok {
			return
		}
		m, e := s.Tasks.Get(id)
		if e != nil {
			errw(w, 404, "task_not_found", "task was not found")
			return
		}
		write(w, 200, map[string]any{"task": m, "runs": s.Tasks.Runs(id)})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		var c map[string]any
		if decode(r, &c) != nil {
			errw(w, 422, "invalid_parameters", "invalid JSON")
			return
		}
		m, e := s.Tasks.Update(id, c)
		if e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				errw(w, 404, "task_not_found", "task was not found")
				return
			}
			errw(w, 422, "invalid_schedule", e.Error())
			return
		}
		write(w, 200, map[string]any{"task": m})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		if !s.Tasks.Delete(id) {
			errw(w, 404, "task_not_found", "task was not found")
			return
		}
		write(w, 200, map[string]string{"status": "deleted"})
		return
	}
	errw(w, 404, "not_found", "not found")
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	key := r.RemoteAddr
	if host, _, err := net.SplitHostPort(key); err == nil {
		key = host
	}
	if s.Limiter.Blocked(key) {
		errw(w, 429, "login_rate_limited", "too many failed login attempts")
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decode(r, &b) != nil {
		errw(w, 422, "invalid_parameters", "invalid JSON")
		return
	}
	u, t, c, expires, e := auth.Login(s.DB, b.Username, b.Password, s.Cfg.SessionTTL)
	if e != nil {
		s.Limiter.Fail(key)
		errw(w, 401, "invalid_credentials", "username or password is incorrect")
		return
	}
	s.Limiter.Clear(key)
	auth.SetCookie(w, s.Cfg, t)
	write(w, 200, map[string]any{"user": u, "csrf_token": c, "expires_at": expires})
}
func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r, false); !ok {
		return
	}
	write(w, 200, map[string]any{"nodes": s.Nodes.List(s.Gateway.OnlineIDs())})
}
func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r, true)
	if !ok {
		return
	}
	var b struct {
		Name   string   `json:"name"`
		Region string   `json:"region"`
		Tags   []string `json:"tags"`
		Warp   string   `json:"warp_adapter"`
		XUI    string   `json:"xui_service"`
		Notes  string   `json:"notes"`
	}
	if decode(r, &b) != nil {
		errw(w, 422, "invalid_parameters", "invalid JSON")
		return
	}
	if b.Warp == "" {
		b.Warp = "generic"
	}
	if b.XUI == "" {
		b.XUI = "x-ui"
	}
	n, t, e, er := s.Nodes.Create(b.Name, b.Region, b.Tags, b.Warp, b.XUI, b.Notes, ss.UserID)
	if er != nil {
		errw(w, 422, "invalid_parameters", er.Error())
		return
	}
	write(w, 200, map[string]any{"node": n, "registration_token": t, "registration_token_expires_at": e})
}
func (s *Server) nodeAPI(w http.ResponseWriter, r *http.Request, p string) {
	parts := strings.Split(strings.TrimPrefix(p, "nodes/"), "/")
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		if _, ok := s.session(w, r, false); !ok {
			return
		}
		n, e := s.Nodes.Get(id)
		if e != nil {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		n["online"] = s.Gateway.Online(id)
		write(w, 200, map[string]any{"node": n})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		if !s.Gateway.DeleteNode(id) {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]string{"status": "deleted"})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		var fields map[string]any
		if decode(r, &fields) != nil {
			errw(w, 422, "invalid_parameters", "invalid JSON")
			return
		}
		n, e := s.Nodes.Update(id, fields)
		if e != nil {
			var validation *nodes.ValidationError
			if errors.As(e, &validation) {
				errw(w, 422, "invalid_parameters", validation.Error())
			} else {
				errw(w, 404, "node_not_found", "node was not found")
			}
			return
		}
		n["online"] = s.Gateway.Online(id)
		write(w, 200, map[string]any{"node": n})
		return
	}
	if len(parts) >= 2 && parts[1] == "enrollment-token" && r.Method == http.MethodPost {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		t, exp, e := s.Nodes.NewEnrollment(id)
		if e != nil {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]string{"registration_token": t, "registration_token_expires_at": exp})
		return
	}
	if len(parts) == 3 && parts[1] == "credentials" && parts[2] == "rotate" && r.Method == http.MethodPost {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		c, yes := s.Gateway.RotateCredential(id)
		if !yes {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]string{"agent_credential": c})
		return
	}
	if len(parts) == 3 && parts[1] == "credentials" && parts[2] == "revoke" && r.Method == http.MethodPost {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		if !s.Gateway.RevokeCredential(id) {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]string{"status": "revoked"})
		return
	}
	if len(parts) >= 2 && parts[1] == "ip-history" {
		if _, ok := s.session(w, r, false); !ok {
			return
		}
		if _, err := s.Nodes.Get(id); err != nil {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]any{"history": s.Nodes.History(id), "limit": 100})
		return
	}
	if len(parts) >= 2 && parts[1] == "xui-backups" {
		s.backupAPI(w, r, id, parts[2:])
		return
	}
	if len(parts) >= 2 && parts[1] == "actions" && r.Method == http.MethodPost {
		s.createAction(w, r, id)
		return
	}
	errw(w, 404, "not_found", "not found")
}
func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r, false); !ok {
		return
	}
	catalog := make([]map[string]any, 0, len(models.Actions))
	for _, name := range models.Actions {
		catalog = append(catalog, map[string]any{"name": name, "state_changing": models.StateChanging[name]})
	}
	write(w, 200, map[string]any{"actions": catalog, "requests": s.Actions.List(r.URL.Query().Get("node_id"))})
}
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r, false); !ok {
		return
	}
	write(w, 200, map[string]any{"requests": s.Actions.List(r.URL.Query().Get("node_id"))})
}
func (s *Server) createAction(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.session(w, r, true); !ok {
		return
	}
	var b struct {
		Action     string         `json:"action"`
		Parameters map[string]any `json:"parameters"`
		RequestID  string         `json:"request_id"`
		Queue      bool           `json:"queue_if_offline"`
	}
	if decode(r, &b) != nil {
		errw(w, 422, "invalid_parameters", "invalid JSON")
		return
	}
	if b.Parameters == nil {
		b.Parameters = map[string]any{}
	}
	if b.Action == "install_xui" {
		if node, err := s.Nodes.Get(id); err == nil && strings.TrimSuffix(node["xui_service"].(string), ".service") == "xray" {
			errw(w, 400, "unsupported_for_xray", "3x-ui installation is not supported on xray nodes")
			return
		}
	}
	if b.Action == "backup_xui" || b.Action == "restore_xui" {
		backupID, ok := b.Parameters["backup_id"].(string)
		if !ok {
			errw(w, 422, "backup_not_found", "a backup belonging to this node is required")
			return
		}
		if _, err := s.Backups.Get(backupID, id); err != nil {
			errw(w, 422, "backup_not_found", "a backup belonging to this node is required")
			return
		}
		if b.Action == "restore_xui" {
			if _, _, err := s.Backups.Ready(backupID, id); err != nil {
				errw(w, 422, err.Error(), err.Error())
				return
			}
		}
	}
	m, e := s.Actions.Create(r.Context(), id, b.Action, b.Parameters, "manual", b.RequestID, b.Queue)
	if e != nil {
		code := e.Error()
		status := 422
		if code == "node_not_found" {
			status = 404
		}
		if code == "action_busy" || code == "request_duplicate" {
			status = 409
		}
		errw(w, status, code, code)
		return
	}
	write(w, 200, map[string]any{"request": m})
}
func (s *Server) backupAPI(w http.ResponseWriter, r *http.Request, node string, tail []string) {
	if len(tail) == 0 && r.Method == http.MethodGet {
		if _, ok := s.session(w, r, false); !ok {
			return
		}
		write(w, 200, map[string]any{"backups": s.Backups.List(node)})
		return
	}
	if len(tail) == 1 && tail[0] == "prepare" && r.Method == http.MethodPost {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		m, e := s.Backups.Prepare(node)
		if e != nil {
			errw(w, 404, "node_not_found", "node was not found")
			return
		}
		write(w, 200, map[string]any{"backup": m})
		return
	}
	if len(tail) == 1 && r.Method == http.MethodDelete {
		if _, ok := s.session(w, r, true); !ok {
			return
		}
		if !s.Backups.Delete(tail[0], node) {
			errw(w, 404, "backup_not_found", "backup was not found")
			return
		}
		write(w, 200, map[string]string{"status": "deleted"})
		return
	}
	errw(w, 404, "not_found", "not found")
}
func (s *Server) AgentBackup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	node := r.Header.Get("X-VPS-Node-ID")
	authz := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if _, ok := s.Nodes.AuthCredential(node, authz); !ok {
		errw(w, 401, "agent_authentication_failed", "agent authentication failed")
		return
	}
	if r.Method == http.MethodPut {
		m, e := s.Backups.Save(id, node, r.Body)
		if e != nil {
			errw(w, 422, e.Error(), e.Error())
			return
		}
		write(w, 200, map[string]any{"backup": m})
		return
	}
	m, p, e := s.Backups.Ready(id, node)
	if e != nil {
		errw(w, 404, e.Error(), e.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", m["filename"]))
	if digest, ok := m["sha256"].(string); ok {
		w.Header().Set("X-VPS-Backup-SHA256", digest)
	}
	http.ServeFile(w, r, p)
}
