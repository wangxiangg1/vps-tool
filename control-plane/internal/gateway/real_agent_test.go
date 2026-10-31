package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/backups"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/gateway"
	"vps-tool/control-plane/internal/httpapi"
	"vps-tool/control-plane/internal/nodes"
	"vps-tool/control-plane/internal/scheduler"
)

// Exercise the actual Agent binary with its existing protocol and a dry-run
// helper; all database, certificate and Agent files live in the test directory.
func TestRealAgentRegistrationReconnectAndStatusAction(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the Agent binary")
	}
	work := t.TempDir()
	binary := os.Getenv("VPS_TOOL_AGENT_BINARY")
	if binary == "" {
		_, file, _, _ := runtime.Caller(0)
		agentDir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "agent"))
		binary = filepath.Join(work, "vps-agent")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		build := exec.Command("go", "build", "-o", binary, "./cmd/vps-agent")
		build.Dir = agentDir
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build Agent: %v\n%s", err, output)
		}
	}
	database, err := db.Open(filepath.Join(work, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = database.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{EnrollmentTTL: 600, ActionTTL: 180, HeartbeatTimeout: 90, AdminUser: "admin", AdminPassword: "test-password-123", CookieName: "session", SessionTTL: 3600, BackupDir: filepath.Join(work, "backups")}
	if err = auth.EnsureAdmin(database, cfg); err != nil {
		t.Fatal(err)
	}
	ns := &nodes.Service{DB: database, Cfg: cfg}
	as := &actions.Service{DB: database, Cfg: cfg, Nodes: ns}
	gw := gateway.New(cfg, ns)
	gw.Actions, as.GW = as, gw
	bs := &backups.Service{DB: database, Cfg: cfg, Nodes: ns}
	ts := &scheduler.TaskService{DB: database, Nodes: ns, Actions: as}
	app := &httpapi.Server{DB: database, Cfg: cfg, Nodes: ns, Actions: as, Gateway: gw, Backups: bs, Tasks: ts, Limiter: auth.NewLimiter()}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent", gw.Handler)
	mux.Handle("/", app)
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	var activeCmd *exec.Cmd
	defer func() {
		if activeCmd != nil {
			_ = activeCmd.Process.Kill()
			_ = activeCmd.Wait()
		}
	}()
	client := server.Client()
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	csrf := ""
	api := func(method, path, body string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
		}
		var result map[string]any
		if err = json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	login := api("POST", "/api/auth/login", `{"username":"admin","password":"test-password-123"}`)
	csrf = login["csrf_token"].(string)
	created := api("POST", "/api/nodes", `{"name":"real-agent"}`)
	nodeID := created["node"].(map[string]any)["id"].(string)
	token := created["registration_token"].(string)
	certPath := filepath.Join(work, "ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(work, "agent.json")
	configData, err := json.Marshal(map[string]any{
		"node_id": nodeID, "registration_token": token, "wss_url": "wss" + strings.TrimPrefix(server.URL, "https") + "/agent",
		"warp_adapter": "dry-run", "xui_unit": "x-ui", "state_path": filepath.Join(work, "journal.json"), "dry_run": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}

	start := func() (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		cmd := exec.Command(binary, "-config", configPath, "-dry-run")
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if !strings.HasPrefix(strings.ToUpper(key), "VPS_AGENT_") && key != "SSL_CERT_FILE" && key != "GODEBUG" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+certPath, "GODEBUG=x509sslcertoverrideplatform=1")
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		activeCmd = cmd
		return cmd, &output
	}
	stop := func(cmd *exec.Cmd) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if activeCmd == cmd {
			activeCmd = nil
		}
	}
	wait := func(condition func() bool) bool {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return true
			}
			time.Sleep(25 * time.Millisecond)
		}
		return false
	}
	cmd, output := start()
	if !wait(func() bool {
		data, err := os.ReadFile(configPath)
		var config map[string]any
		return err == nil && json.Unmarshal(data, &config) == nil && config["credential"] != "" && config["credential"] != nil && gw.Online(nodeID)
	}) {
		stop(cmd)
		t.Fatalf("Agent did not register: %s", output.String())
	}
	if !wait(func() bool {
		n, err := ns.Get(nodeID)
		return err == nil && n["warp_status"] == "on" && n["xui_status"] == "running"
	}) {
		stop(cmd)
		t.Fatalf("Agent did not report its status: %s", output.String())
	}
	request := api("POST", "/api/nodes/"+nodeID+"/actions", `{"action":"get_status","parameters":{}}`)["request"].(map[string]any)
	if !wait(func() bool {
		row, err := as.Get(request["id"].(string))
		return err == nil && row["status"] == "succeeded"
	}) {
		stop(cmd)
		t.Fatalf("Agent did not finish get_status: %s", output.String())
	}
	stop(cmd)
	if !wait(func() bool { return !gw.Online(nodeID) }) {
		t.Fatal("Agent remained online after stopping")
	}

	cmd, output = start()
	if !wait(func() bool { return gw.Online(nodeID) }) {
		stop(cmd)
		t.Fatalf("Agent did not reconnect with saved credential: %s", output.String())
	}
	request = api("POST", "/api/nodes/"+nodeID+"/actions", `{"action":"get_status","parameters":{}}`)["request"].(map[string]any)
	if !wait(func() bool {
		row, err := as.Get(request["id"].(string))
		return err == nil && row["status"] == "succeeded"
	}) {
		stop(cmd)
		t.Fatalf("Agent did not finish get_status after reconnect: %s", output.String())
	}
	stop(cmd)
}
