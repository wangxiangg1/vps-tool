package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/nodes"
	"vps-tool/control-plane/internal/scheduler"
)

func TestTaskEndpointsRequireCSRFAndCRUD(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{AdminUser: "admin", AdminPassword: "long-password-123", CookieName: "session", SessionTTL: 3600, ActionTTL: 180}
	if err = auth.EnsureAdmin(d, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES('node-1','node','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	tasks := &scheduler.TaskService{DB: d, Nodes: ns, Actions: as}
	s := &Server{DB: d, Cfg: cfg, Tasks: tasks, Limiter: auth.NewLimiter()}
	do := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	login := do("POST", "/api/auth/login", `{"username":"admin","password":"long-password-123"}`, nil, "")
	if login.Code != 200 {
		t.Fatalf("login %d: %s", login.Code, login.Body.String())
	}
	var loginBody struct {
		CSRF string `json:"csrf_token"`
	}
	if err = json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("missing cookie")
	}
	body := `{"name":"daily","node_ids":["node-1"],"action":"warp_on","parameters":{},"schedule_type":"daily","schedule_value":"10:00","timezone":"UTC"}`
	if w := do("POST", "/api/tasks", body, cookies[0], ""); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	w := do("POST", "/api/tasks", body, cookies[0], loginBody.CSRF)
	if w.Code != 200 {
		t.Fatalf("create %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Task struct {
			ID         string `json:"id"`
			Enabled    bool   `json:"enabled"`
			MaxRetries int    `json:"max_retries"`
		} `json:"task"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Task.Enabled || created.Task.MaxRetries != 2 {
		t.Fatalf("wrong defaults: %+v", created.Task)
	}
	path := "/api/tasks/" + created.Task.ID
	if w = do("GET", path, "", cookies[0], ""); w.Code != 200 {
		t.Fatalf("get %d: %s", w.Code, w.Body.String())
	}
	if w = do("PATCH", path, `{"enabled":false}`, cookies[0], loginBody.CSRF); w.Code != 200 {
		t.Fatalf("patch %d: %s", w.Code, w.Body.String())
	}
	if w = do("DELETE", path, "", cookies[0], loginBody.CSRF); w.Code != 200 {
		t.Fatalf("delete %d: %s", w.Code, w.Body.String())
	}
	if w = do("GET", path, "", cookies[0], ""); w.Code != 404 {
		t.Fatalf("deleted task status=%d", w.Code)
	}
}
