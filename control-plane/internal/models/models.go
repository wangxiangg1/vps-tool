package models

import "encoding/json"

var Actions = []string{"get_status", "get_ip", "warp_on", "warp_off", "change_ip", "restart_xui", "upgrade_agent", "install_warp", "install_xui", "backup_xui", "restore_xui"}
var StateChanging = map[string]bool{"warp_on": true, "warp_off": true, "change_ip": true, "restart_xui": true, "upgrade_agent": true, "install_warp": true, "install_xui": true, "backup_xui": true, "restore_xui": true}

type Envelope struct {
	ProtocolVersion int             `json:"protocol_version"`
	MessageType     string          `json:"message_type"`
	MessageID       string          `json:"message_id"`
	SentAt          string          `json:"sent_at"`
	Payload         json.RawMessage `json:"payload"`
}
type Node struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Region             string         `json:"region"`
	Tags               []string       `json:"tags"`
	WarpAdapter        string         `json:"warp_adapter"`
	XUIService         string         `json:"xui_service"`
	Notes              string         `json:"notes"`
	AgentVersion       *string        `json:"agent_version"`
	Hostname           *string        `json:"hostname"`
	OSName             *string        `json:"os_name"`
	OSVersion          *string        `json:"os_version"`
	Architecture       *string        `json:"architecture"`
	CPUPercent         *float64       `json:"cpu_percent"`
	MemoryUsed         *int64         `json:"memory_used_bytes"`
	MemoryTotal        *int64         `json:"memory_total_bytes"`
	RootUsed           *int64         `json:"root_used_bytes"`
	RootTotal          *int64         `json:"root_total_bytes"`
	Uptime             *int64         `json:"uptime_seconds"`
	WarpStatus         string         `json:"warp_status"`
	XUIStatus          string         `json:"xui_status"`
	PublicIPv4         *string        `json:"public_ipv4"`
	PublicIPv6         *string        `json:"public_ipv6"`
	Status             map[string]any `json:"status"`
	LastStatusAt       *string        `json:"last_status_at"`
	LastSeenAt         *string        `json:"last_seen_at"`
	LastIPChangeAt     *string        `json:"last_ip_change_at"`
	LastIPChangeResult any            `json:"last_ip_change_result"`
	CreatedAt          string         `json:"created_at"`
	UpdatedAt          string         `json:"updated_at"`
	Online             bool           `json:"online,omitempty"`
}
