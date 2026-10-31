package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/models"
	"vps-tool/control-plane/internal/nodes"
)

type Conn struct {
	NodeID       string
	WS           *websocket.Conn
	Mu           sync.Mutex
	lastReceived time.Time // guarded by Gateway.Mu
}
type Gateway struct {
	Cfg      config.Settings
	Nodes    *nodes.Service
	Actions  *actions.Service
	Mu       sync.RWMutex
	Conns    map[string]*Conn
	Upgrader websocket.Upgrader
}

func New(c config.Settings, n *nodes.Service) *Gateway {
	return &Gateway{Cfg: c, Nodes: n, Conns: map[string]*Conn{}, Upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}}
}
func (g *Gateway) Online(id string) bool {
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	c := g.Conns[id]
	return c != nil && time.Since(c.lastReceived) < g.heartbeatTimeout()
}
func (g *Gateway) heartbeatTimeout() time.Duration {
	if g.Cfg.HeartbeatTimeout <= 0 {
		return 90 * time.Second
	}
	return time.Duration(g.Cfg.HeartbeatTimeout) * time.Second
}
func (g *Gateway) OnlineIDs() map[string]bool {
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	m := map[string]bool{}
	for k, c := range g.Conns {
		if time.Since(c.lastReceived) < g.heartbeatTimeout() {
			m[k] = true
		}
	}
	return m
}
func (g *Gateway) Send(ctx context.Context, m map[string]any) error {
	node, _ := m["node_id"].(string)
	g.Mu.RLock()
	c := g.Conns[node]
	if c != nil && time.Since(c.lastReceived) >= g.heartbeatTimeout() {
		c = nil
	}
	g.Mu.RUnlock()
	if c == nil {
		return fmt.Errorf("agent offline")
	}
	p := map[string]any{"request_id": m["id"], "node_id": node, "action": m["action"], "issued_at": m["issued_at"], "deadline_at": m["deadline_at"], "parameters": m["parameters"]}
	b, _ := json.Marshal(p)
	e := map[string]any{"protocol_version": 1, "message_type": "command", "message_id": uuid.NewString(), "sent_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": json.RawMessage(b)}
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if err := c.WS.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return c.WS.WriteJSON(e)
}
func (g *Gateway) Handler(w http.ResponseWriter, r *http.Request) {
	ws, err := g.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Conn{WS: ws}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	fail := func(code, message string) {
		_ = g.send(c, "server_notice", map[string]string{"error_code": code, "message": message})
	}
	env, err := readEnvelope(ws)
	if err != nil || env.MessageType != "agent_hello" {
		fail("handshake_failed", "invalid or missing agent handshake")
		return
	}
	var hello helloPayload
	if decodeStrict(env.Payload, &hello) != nil || hello.NodeID == "" || len(hello.Capabilities) > 20 || len(hello.Reconcile) > 200 {
		fail("handshake_failed", "invalid agent hello")
		return
	}
	if hello.ProtocolVersion != nil && *hello.ProtocolVersion != 1 {
		fail("agent_version_incompatible", "unsupported protocol version")
		return
	}
	for _, capability := range hello.Capabilities {
		if !contains(models.Actions, capability) {
			fail("unsupported_capability", "unsupported agent capability")
			return
		}
	}
	headerCredential := authorizationCredential(r.Header.Get("Authorization"))
	if (hello.RegistrationToken != "" && (hello.Credential != "" || headerCredential != "")) || (hello.Credential != "" && headerCredential != "" && subtle.ConstantTimeCompare([]byte(hello.Credential), []byte(headerCredential)) != 1) {
		fail("handshake_failed", "credential sources do not match")
		return
	}
	credential := hello.Credential
	if credential == "" {
		credential = headerCredential
	}
	var node map[string]any
	var cred string
	// Serialize credential changes and connection registration for this gateway.
	g.Mu.Lock()
	if hello.RegistrationToken != "" {
		node, cred, err = g.Nodes.Enroll(hello.NodeID, hello.RegistrationToken)
	} else if credential != "" {
		node, _ = g.Nodes.AuthCredential(hello.NodeID, credential)
	}
	if err != nil || node == nil {
		g.Mu.Unlock()
		fail("authentication_failed", "agent authentication failed")
		return
	}
	c.NodeID = hello.NodeID
	// Hold the new connection's writer lock until its authentication notice is
	// sent; dispatchers may discover it but cannot overtake that notice.
	c.Mu.Lock()
	old := g.Conns[c.NodeID]
	c.lastReceived = time.Now()
	g.Conns[c.NodeID] = c
	g.Mu.Unlock()
	defer g.detach(c)
	if old != nil {
		_ = old.WS.Close()
		_, _ = g.Nodes.DB.SQL.Exec(`INSERT INTO audit_logs(id,actor_type,actor_id,event_type,node_id,metadata_json,created_at) VALUES(?,?,?,?,?,'{}',?)`, uuid.NewString(), "agent", c.NodeID, "agent_connection_replaced", c.NodeID, time.Now().UTC().Format(time.RFC3339Nano))
	}
	err = g.sendLocked(c, "server_notice", map[string]any{"event": "agent_authenticated", "node_id": c.NodeID, "registered_credential": cred})
	c.Mu.Unlock()
	if err != nil {
		return
	}
	g.Nodes.Seen(c.NodeID)
	for _, raw := range hello.Reconcile {
		var p resultPayload
		if decodeStrict(raw, &p) == nil && p.RequestID != "" && p.Success != nil {
			if err := g.Actions.Finish(c.NodeID, p.RequestID, *p.Success, p.Result, p.ErrorCode, p.ErrorMessage); err != nil {
				log.Printf("agent reconciliation failed: %v", err)
			}
		}
	}
	if err := g.Actions.DispatchPending(r.Context(), c.NodeID); err != nil {
		log.Printf("agent pending dispatch failed: %v", err)
	}
	for {
		_ = ws.SetReadDeadline(time.Now().Add(g.heartbeatTimeout()))
		env, err := readEnvelope(ws)
		if err != nil {
			return
		}
		g.Mu.Lock()
		c.lastReceived = time.Now()
		g.Mu.Unlock()
		if err := g.handleMessage(c, env); err != nil {
			fail("invalid_message", "invalid agent message")
			return
		}
	}
}

func authorizationCredential(value string) string {
	scheme, credential, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

func (g *Gateway) detach(c *Conn) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if g.Conns[c.NodeID] == c {
		delete(g.Conns, c.NodeID)
		if err := g.Actions.MarkUnknownForNode(c.NodeID); err != nil {
			log.Printf("agent disconnect: %v", err)
		}
	}

}
func (g *Gateway) Disconnect(id string) {
	g.Mu.Lock()
	c := g.disconnectLocked(id)
	g.Mu.Unlock()
	if c != nil {
		_ = c.WS.Close()
	}
}
func (g *Gateway) disconnectLocked(id string) *Conn {
	c := g.Conns[id]
	if c != nil {
		delete(g.Conns, id)
		if err := g.Actions.MarkUnknownForNode(id); err != nil {
			log.Printf("agent disconnect: %v", err)
		}
	}
	return c
}

func (g *Gateway) DeleteNode(id string) bool {
	g.Mu.Lock()
	ok := g.Nodes.Delete(id)
	var c *Conn
	if ok {
		c = g.disconnectLocked(id)
	}
	g.Mu.Unlock()
	if c != nil {
		_ = c.WS.Close()
	}
	return ok
}
func (g *Gateway) RotateCredential(id string) (string, bool) {
	g.Mu.Lock()
	credential, ok := g.Nodes.Rotate(id)
	var c *Conn
	if ok {
		c = g.disconnectLocked(id)
	}
	g.Mu.Unlock()
	if c != nil {
		_ = c.WS.Close()
	}
	return credential, ok
}
func (g *Gateway) RevokeCredential(id string) bool {
	g.Mu.Lock()
	ok := g.Nodes.Revoke(id)
	var c *Conn
	if ok {
		c = g.disconnectLocked(id)
	}
	g.Mu.Unlock()
	if c != nil {
		_ = c.WS.Close()
	}
	return ok
}
func (g *Gateway) send(c *Conn, typ string, p any) error {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	return g.sendLocked(c, typ, p)
}
func (g *Gateway) sendLocked(c *Conn, typ string, p any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := c.WS.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return c.WS.WriteJSON(map[string]any{"protocol_version": 1, "message_type": typ, "message_id": uuid.NewString(), "sent_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": json.RawMessage(b)})
}

var _ actions.Gateway = (*Gateway)(nil)
