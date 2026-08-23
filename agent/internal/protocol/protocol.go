package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"vps-tool/agent/internal/validation"
)

const Version = 1

const (
	MessageAgentHello      = "agent_hello"
	MessageHeartbeat       = "heartbeat"
	MessageStatusReport    = "status_report"
	MessageCommand         = "command"
	MessageCommandAck      = "command_ack"
	MessageCommandProgress = "command_progress"
	MessageCommandResult   = "command_result"
	MessageServerNotice    = "server_notice"
)

var actionSet = map[string]bool{
	"get_status":  false,
	"get_ip":      false,
	"warp_on":     true,
	"warp_off":    true,
	"change_ip":   true,
	"restart_xui": true,
}

type Envelope struct {
	ProtocolVersion int             `json:"protocol_version"`
	MessageType     string          `json:"message_type"`
	MessageID       string          `json:"message_id"`
	SentAt          time.Time       `json:"sent_at"`
	Payload         json.RawMessage `json:"payload"`
}

type Command struct {
	RequestID  string          `json:"request_id"`
	NodeID     string          `json:"node_id"`
	Action     string          `json:"action"`
	IssuedAt   time.Time       `json:"issued_at"`
	DeadlineAt time.Time       `json:"deadline_at"`
	Parameters json.RawMessage `json:"parameters"`
}

type ChangeIPParameters struct {
	MaxAttempts    *int `json:"max_attempts,omitempty"`
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
}

type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func New(messageType string, payload any) (Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal payload: %w", err)
	}
	return Envelope{
		ProtocolVersion: Version,
		MessageType:     messageType,
		MessageID:       NewMessageID(),
		SentAt:          time.Now().UTC(),
		Payload:         data,
	}, nil
}

func NewMessageID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func Marshal(envelope Envelope) ([]byte, error) {
	if envelope.ProtocolVersion == 0 {
		envelope.ProtocolVersion = Version
	}
	if envelope.MessageID == "" {
		envelope.MessageID = NewMessageID()
	}
	if envelope.SentAt.IsZero() {
		envelope.SentAt = time.Now().UTC()
	}
	if len(envelope.Payload) == 0 {
		envelope.Payload = json.RawMessage(`{}`)
	}
	return json.Marshal(envelope)
}

func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := decodeStrict(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if envelope.ProtocolVersion != Version {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", envelope.ProtocolVersion)
	}
	if envelope.MessageType == "" || envelope.MessageID == "" || envelope.SentAt.IsZero() || len(bytes.TrimSpace(envelope.Payload)) == 0 {
		return Envelope{}, errors.New("envelope is missing required fields")
	}
	return envelope, nil
}

func DecodeCommand(envelope Envelope) (Command, error) {
	if envelope.ProtocolVersion != Version {
		return Command{}, fmt.Errorf("unsupported protocol version %d", envelope.ProtocolVersion)
	}
	if envelope.MessageType != MessageCommand {
		return Command{}, fmt.Errorf("unexpected message type %q", envelope.MessageType)
	}
	var command Command
	if err := decodeStrict(envelope.Payload, &command); err != nil {
		return Command{}, fmt.Errorf("decode command: %w", err)
	}
	if strings.TrimSpace(string(command.Parameters)) == "" {
		return Command{}, errors.New("command parameters are required")
	}
	return command, nil
}

func IsStateChanging(action string) bool { return actionSet[action] }

func Actions() []string {
	return []string{"get_status", "get_ip", "warp_on", "warp_off", "change_ip", "restart_xui"}
}

func ValidateCommand(command Command, expectedNodeID string, now time.Time) error {
	if err := validation.RequestID(command.RequestID); err != nil {
		return &ValidationError{Code: "invalid_parameters", Message: err.Error()}
	}
	if command.NodeID != expectedNodeID {
		return &ValidationError{Code: "invalid_parameters", Message: "command node_id does not match this agent"}
	}
	if command.Action == "" {
		return &ValidationError{Code: "invalid_parameters", Message: "action is required"}
	}
	if _, ok := actionSet[command.Action]; !ok {
		return &ValidationError{Code: "unsupported_action", Message: "action is not supported by this agent"}
	}
	if command.IssuedAt.IsZero() || command.DeadlineAt.IsZero() || !command.DeadlineAt.After(command.IssuedAt) {
		return &ValidationError{Code: "invalid_parameters", Message: "issued_at and deadline_at are invalid"}
	}
	if !now.IsZero() && now.After(command.DeadlineAt) {
		return &ValidationError{Code: "request_expired", Message: "command deadline has passed"}
	}
	if err := validateParameters(command.Action, command.Parameters); err != nil {
		return err
	}
	return nil
}

func DecodeChangeIPParameters(raw []byte) (ChangeIPParameters, error) {
	var parameters ChangeIPParameters
	if err := decodeObject(raw, &parameters); err != nil {
		return ChangeIPParameters{}, &ValidationError{Code: "invalid_parameters", Message: err.Error()}
	}
	if parameters.MaxAttempts != nil && (*parameters.MaxAttempts < 1 || *parameters.MaxAttempts > 3) {
		return ChangeIPParameters{}, &ValidationError{Code: "invalid_parameters", Message: "max_attempts must be between 1 and 3"}
	}
	if parameters.TimeoutSeconds != nil && (*parameters.TimeoutSeconds < 30 || *parameters.TimeoutSeconds > 180) {
		return ChangeIPParameters{}, &ValidationError{Code: "invalid_parameters", Message: "timeout_seconds must be between 30 and 180"}
	}
	return parameters, nil
}

func validateParameters(action string, raw []byte) error {
	switch action {
	case "change_ip":
		_, err := DecodeChangeIPParameters(raw)
		return err
	default:
		if err := decodeObject(raw, &struct{}{}); err != nil {
			return &ValidationError{Code: "invalid_parameters", Message: err.Error()}
		}
		return nil
	}
}

func decodeObject(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("parameters must be a JSON object")
	}
	return decodeStrict(trimmed, target)
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
