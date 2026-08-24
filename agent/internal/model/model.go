package model

// WarpState is the normalized state reported by a supported WARP adapter.
type WarpState string

const (
	WarpOn       WarpState = "on"
	WarpOff      WarpState = "off"
	WarpDegraded WarpState = "degraded"
	WarpUnknown  WarpState = "unknown"
)

func (s WarpState) Valid() bool {
	switch s {
	case WarpOn, WarpOff, WarpDegraded, WarpUnknown:
		return true
	default:
		return false
	}
}

type WarpSnapshot struct {
	State WarpState `json:"warp_state"`
	IPv4  string    `json:"ipv4,omitempty"`
	IPv6  string    `json:"ipv6,omitempty"`
}

// XUIState is the normalized service state reported by the fixed helper.
type XUIState string

const (
	XUIRunning  XUIState = "running"
	XUIStopped  XUIState = "stopped"
	XUIFailed   XUIState = "failed"
	XUINotFound XUIState = "not_found"
	XUIUnknown  XUIState = "unknown"
)

func (s XUIState) Valid() bool {
	switch s {
	case XUIRunning, XUIStopped, XUIFailed, XUINotFound, XUIUnknown:
		return true
	default:
		return false
	}
}

type XUISnapshot struct {
	State XUIState `json:"xui_state"`
}
