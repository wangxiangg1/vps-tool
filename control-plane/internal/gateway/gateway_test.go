package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/models"
	"vps-tool/control-plane/internal/nodes"
)

func TestAgentLifecycleAndQueuedAction(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{EnrollmentTTL: 600, ActionTTL: 180, HeartbeatTimeout: 90}
	ns := &nodes.Service{DB: database, Cfg: cfg}
	as := &actions.Service{DB: database, Cfg: cfg, Nodes: ns}
	gw := New(cfg, ns)
	gw.Actions, as.GW = as, gw
	node, token, _, err := ns.Create("test-node", "", []string{}, "generic", "x-ui", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := node["id"].(string)
	server := httptest.NewServer(http.HandlerFunc(gw.Handler))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/agent"

	dial := func(credential string) *websocket.Conn {
		t.Helper()
		header := http.Header{}
		if credential != "" {
			header.Set("Authorization", "Bearer "+credential)
		}
		conn, _, err := websocket.DefaultDialer.Dial(url, header)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	send := func(conn *websocket.Conn, typ string, payload any) {
		t.Helper()
		if err := conn.WriteJSON(map[string]any{"protocol_version": 1, "message_type": typ, "message_id": "test-message", "sent_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": payload}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(conn *websocket.Conn, typ string) map[string]any {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var env models.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatal(err)
		}
		if env.MessageType != typ {
			t.Fatalf("message type = %s, want %s", env.MessageType, typ)
		}
		var payload map[string]any
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	wait := func(description string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", description)
	}
	waitStatus := func(requestID, expected string) {
		t.Helper()
		wait(expected, func() bool { m, err := as.Get(requestID); return err == nil && m["status"] == expected })
	}
	conn := dial("")
	send(conn, "agent_hello", map[string]any{"node_id": id, "registration_token": token, "protocol_version": 1, "agent_version": "test", "capabilities": []string{"get_status"}, "reconcile": []any{}})
	credential, ok := read(conn, "server_notice")["registered_credential"].(string)
	if !ok || credential == "" {
		t.Fatal("missing issued credential")
	}
	wait("agent online", func() bool { return gw.Online(id) })
	send(conn, "status_report", map[string]any{"node_id": id, "protocol_version": 1, "warp_status": "on", "xui_status": "running", "hostname": "sample", "memory_used_bytes": 1234, "public_ipv4": "192.0.2.1"})
	wait("node status", func() bool {
		n, err := ns.Get(id)
		return err == nil && n["warp_status"] == "on" && n["xui_status"] == "running" && n["hostname"] == "sample" && n["memory_used_bytes"] == float64(1234)
	})
	listed := ns.List(gw.OnlineIDs())
	if len(listed) != 1 || listed[0]["warp_status"] != "on" || listed[0]["memory_used_bytes"] != float64(1234) {
		t.Fatalf("node list: %+v", listed)
	}

	created, err := as.Create(context.Background(), id, "get_status", map[string]any{}, "manual", "", false)
	if err != nil {
		t.Fatal(err)
	}
	requestID := created["id"].(string)
	command := read(conn, "command")
	if command["request_id"] != requestID || command["action"] != "get_status" {
		t.Fatalf("command: %+v", command)
	}
	send(conn, "command_ack", map[string]any{"request_id": requestID, "accepted": true})
	waitStatus(requestID, "accepted")
	send(conn, "command_progress", map[string]any{"request_id": requestID, "progress_percent": 50})
	waitStatus(requestID, "running")
	send(conn, "command_result", map[string]any{"request_id": requestID, "success": true, "result": map[string]any{"sample": "ok"}})
	waitStatus(requestID, "succeeded")

	inflight, err := as.Create(context.Background(), id, "get_status", map[string]any{}, "manual", "", false)
	if err != nil {
		t.Fatal(err)
	}
	inflightID := inflight["id"].(string)
	read(conn, "command")
	send(conn, "command_ack", map[string]any{"request_id": inflightID, "accepted": true})
	waitStatus(inflightID, "accepted")
	_ = conn.Close()
	waitStatus(inflightID, "unknown")
	wait("agent offline", func() bool { return !gw.Online(id) })
	queued, err := as.Create(context.Background(), id, "get_status", map[string]any{}, "manual", "", true)
	if err != nil {
		t.Fatal(err)
	}
	queuedID := queued["id"].(string)
	if queued["status"] != "queued" {
		t.Fatalf("queued status = %v", queued["status"])
	}

	conn = dial(credential)
	defer conn.Close()
	send(conn, "agent_hello", map[string]any{"node_id": id, "protocol_version": 1, "agent_version": "test", "capabilities": []string{"get_status"}, "reconcile": []any{map[string]any{"request_id": inflightID, "success": false, "error_code": "sample_error", "error_message": "sample", "result": map[string]any{"sample": "result"}}}})
	read(conn, "server_notice")
	command = read(conn, "command")
	if command["request_id"] != queuedID {
		t.Fatalf("queued command: %+v", command)
	}
	waitStatus(inflightID, "failed")
	finished, err := as.Get(inflightID)
	if err != nil || finished["error_code"] != "sample_error" || finished["result"].(map[string]any)["sample"] != "result" {
		t.Fatalf("reconciled result: %+v %v", finished, err)
	}
	send(conn, "command_ack", map[string]any{"request_id": queuedID, "accepted": true})
	send(conn, "command_result", map[string]any{"request_id": queuedID, "success": true, "result": map[string]any{}})
	waitStatus(queuedID, "succeeded")
	var resultCount int
	if err := database.SQL.QueryRow("SELECT count(*) FROM action_results WHERE request_id=?", queuedID).Scan(&resultCount); err != nil || resultCount != 1 {
		t.Fatalf("result count %d: %v", resultCount, err)
	}
}
