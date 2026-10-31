package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/backups"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/gateway"
	"vps-tool/control-plane/internal/nodes"
)

func TestFrontendAndAgentBackupLifecycle(t *testing.T) {
	tmp := t.TempDir()
	database, err := db.Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = database.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{AdminUser: "admin", AdminPassword: "long-password-123", CookieName: "session", SessionTTL: 3600, EnrollmentTTL: 600, BackupDir: filepath.Join(tmp, "backups")}
	if err = auth.EnsureAdmin(database, cfg); err != nil {
		t.Fatal(err)
	}
	ns := &nodes.Service{DB: database, Cfg: cfg}
	gw := gateway.New(cfg, ns)
	bs := &backups.Service{DB: database, Cfg: cfg, Nodes: ns}
	s := &Server{DB: database, Cfg: cfg, Nodes: ns, Gateway: gw, Backups: bs, Limiter: auth.NewLimiter()}
	node, token, _, err := ns.Create("node", "", []string{}, "generic", "x-ui", "", "")
	if err != nil {
		t.Fatal(err)
	}
	nodeID := node["id"].(string)
	_, credential, err := ns.Enroll(nodeID, token)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path string, body []byte, cookie *http.Cookie, csrf string, agent bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		if agent {
			req.Header.Set("Authorization", "Bearer "+credential)
			req.Header.Set("X-VPS-Node-ID", nodeID)
		}
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/api/agent/xui-backups/") {
			s.AgentBackup(w, req, strings.TrimPrefix(path, "/api/agent/xui-backups/"))
		} else {
			s.ServeHTTP(w, req)
		}
		return w
	}
	login := do("POST", "/api/auth/login", []byte(`{"username":"admin","password":"long-password-123"}`), nil, "", false)
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var loginBody struct {
		CSRF string `json:"csrf_token"`
	}
	if err = json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	cookie := login.Result().Cookies()[0]
	base := "/api/nodes/" + nodeID + "/xui-backups"
	prepared := do("POST", base+"/prepare", nil, cookie, loginBody.CSRF, false)
	if prepared.Code != 200 {
		t.Fatalf("prepare: %d %s", prepared.Code, prepared.Body.String())
	}
	var result struct {
		Backup struct {
			ID     string  `json:"id"`
			Status string  `json:"status"`
			SHA    *string `json:"sha256"`
		} `json:"backup"`
	}
	if err = json.Unmarshal(prepared.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Backup.ID == "" || result.Backup.Status != "pending" || result.Backup.SHA != nil {
		t.Fatalf("pending backup: %+v", result.Backup)
	}
	listed := do("GET", base, nil, cookie, "", false)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), result.Backup.ID) {
		t.Fatalf("pending list: %d %s", listed.Code, listed.Body.String())
	}

	contents := []byte("test backup contents")
	agentPath := "/api/agent/xui-backups/" + result.Backup.ID
	upload := do("PUT", agentPath, contents, nil, "", true)
	if upload.Code != 200 {
		t.Fatalf("upload: %d %s", upload.Code, upload.Body.String())
	}
	download := do("GET", agentPath, nil, nil, "", true)
	digest := sha256.Sum256(contents)
	if download.Code != 200 || !bytes.Equal(download.Body.Bytes(), contents) || download.Header().Get("X-VPS-Backup-SHA256") != hex.EncodeToString(digest[:]) {
		t.Fatalf("download: %d %q checksum=%s", download.Code, download.Body.Bytes(), download.Header().Get("X-VPS-Backup-SHA256"))
	}
	deleted := do("DELETE", base+"/"+result.Backup.ID, nil, cookie, loginBody.CSRF, false)
	if deleted.Code != 200 {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body.String())
	}
}
