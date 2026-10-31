package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/gorilla/websocket"
	"vps-tool/control-plane/internal/models"
)

type helloPayload struct {
	NodeID            string            `json:"node_id"`
	Credential        string            `json:"credential"`
	RegistrationToken string            `json:"registration_token"`
	ProtocolVersion   *int              `json:"protocol_version"`
	AgentVersion      string            `json:"agent_version"`
	Hostname          *string           `json:"hostname"`
	Architecture      *string           `json:"architecture"`
	Capabilities      []string          `json:"capabilities"`
	Reconcile         []json.RawMessage `json:"reconcile"`
}

type resultPayload struct {
	RequestID    string         `json:"request_id"`
	Success      *bool          `json:"success"`
	ErrorCode    string         `json:"error_code"`
	ErrorMessage string         `json:"error_message"`
	Result       map[string]any `json:"result"`
}

type statusPayload struct {
	NodeID           string   `json:"node_id"`
	ProtocolVersion  *int     `json:"protocol_version,omitempty"`
	AgentVersion     *string  `json:"agent_version,omitempty"`
	Hostname         *string  `json:"hostname,omitempty"`
	OSName           *string  `json:"os_name,omitempty"`
	OSVersion        *string  `json:"os_version,omitempty"`
	Architecture     *string  `json:"architecture,omitempty"`
	CPUPercent       *float64 `json:"cpu_percent,omitempty"`
	MemoryUsedBytes  *int64   `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes *int64   `json:"memory_total_bytes,omitempty"`
	RootUsedBytes    *int64   `json:"root_used_bytes,omitempty"`
	RootTotalBytes   *int64   `json:"root_total_bytes,omitempty"`
	UptimeSeconds    *int64   `json:"uptime_seconds,omitempty"`
	WarpStatus       *string  `json:"warp_status,omitempty"`
	XUIStatus        *string  `json:"xui_status,omitempty"`
	PublicIPv4       *string  `json:"public_ipv4,omitempty"`
	PublicIPv6       *string  `json:"public_ipv6,omitempty"`
	ObservedAt       *string  `json:"observed_at,omitempty"`
}

func decodeStrict(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("expected JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func readEnvelope(ws *websocket.Conn) (models.Envelope, error) {
	var env models.Envelope
	kind, raw, err := ws.ReadMessage()
	if err != nil {
		return env, err
	}
	if kind != websocket.TextMessage {
		return env, errors.New("expected a text message")
	}
	if err := decodeStrict(raw, &env); err != nil {
		return env, err
	}
	if env.ProtocolVersion != 1 {
		return env, errors.New("unsupported protocol version")
	}
	if env.MessageType == "" || env.MessageID == "" || env.SentAt == "" || len(env.Payload) == 0 {
		return env, errors.New("missing envelope fields")
	}
	return env, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (g *Gateway) handleMessage(c *Conn, env models.Envelope) error {
	switch env.MessageType {
	case "heartbeat":
		var p struct {
			NodeID            string  `json:"node_id"`
			WarpState         *string `json:"warp_state"`
			XUIState          *string `json:"xui_state"`
			UptimeSeconds     *int64  `json:"uptime_seconds"`
			StatusCollectedAt *string `json:"status_collected_at"`
		}
		if err := decodeStrict(env.Payload, &p); err != nil || p.NodeID != c.NodeID {
			return errors.New("invalid heartbeat")
		}
		g.Nodes.Seen(c.NodeID)
	case "status_report":
		var p statusPayload
		if err := decodeStrict(env.Payload, &p); err != nil || p.NodeID != c.NodeID || (p.ProtocolVersion != nil && *p.ProtocolVersion != 1) {
			return errors.New("invalid status report")
		}
		if p.CPUPercent != nil && (*p.CPUPercent < 0 || *p.CPUPercent > 100) {
			return errors.New("invalid CPU percentage")
		}
		for _, value := range []*int64{p.MemoryUsedBytes, p.MemoryTotalBytes, p.RootUsedBytes, p.RootTotalBytes, p.UptimeSeconds} {
			if value != nil && *value < 0 {
				return errors.New("invalid status counter")
			}
		}
		if p.WarpStatus != nil && !contains([]string{"on", "off", "degraded", "unknown"}, *p.WarpStatus) {
			return errors.New("invalid warp status")
		}
		if p.XUIStatus != nil && !contains([]string{"running", "stopped", "failed", "not_found", "unknown"}, *p.XUIStatus) {
			return errors.New("invalid service status")
		}
		b, _ := json.Marshal(p)
		var values map[string]any
		_ = json.Unmarshal(b, &values)
		return g.Nodes.UpdateStatus(c.NodeID, values)
	case "command_ack":
		var p struct {
			RequestID    string `json:"request_id"`
			Accepted     *bool  `json:"accepted"`
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}
		if decodeStrict(env.Payload, &p) != nil || p.RequestID == "" || p.Accepted == nil {
			return errors.New("invalid acknowledgment")
		}
		if *p.Accepted {
			return g.Actions.Accept(c.NodeID, p.RequestID)
		}
		if p.ErrorCode == "" {
			p.ErrorCode = "agent_rejected"
		}
		if p.ErrorMessage == "" {
			p.ErrorMessage = "Agent rejected the action"
		}
		return g.Actions.Finish(c.NodeID, p.RequestID, false, nil, p.ErrorCode, p.ErrorMessage)
	case "command_progress":
		var p struct {
			RequestID       string  `json:"request_id"`
			ProgressPercent *int    `json:"progress_percent"`
			Message         *string `json:"message"`
		}
		if decodeStrict(env.Payload, &p) != nil || p.RequestID == "" || p.ProgressPercent == nil || *p.ProgressPercent < 0 || *p.ProgressPercent > 100 {
			return errors.New("invalid progress")
		}
		return g.Actions.Progress(c.NodeID, p.RequestID)
	case "command_result":
		var p resultPayload
		if decodeStrict(env.Payload, &p) != nil || p.RequestID == "" || p.Success == nil {
			return errors.New("invalid result")
		}
		return g.Actions.Finish(c.NodeID, p.RequestID, *p.Success, p.Result, p.ErrorCode, p.ErrorMessage)
	default:
		return g.send(c, "server_notice", map[string]string{"error_code": "unsupported_message_type", "message": "unsupported message type"})
	}
	return nil
}
