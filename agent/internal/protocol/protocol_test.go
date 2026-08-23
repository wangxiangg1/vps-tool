package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidateCommandActionAllowlist(t *testing.T) {
	now := time.Now().UTC()
	base := Command{
		RequestID:  "request-1",
		NodeID:     "node-1",
		IssuedAt:   now.Add(-time.Second),
		DeadlineAt: now.Add(time.Minute),
		Parameters: json.RawMessage(`{}`),
	}
	for _, action := range Actions() {
		base.Action = action
		if err := ValidateCommand(base, "node-1", now); err != nil {
			t.Fatalf("supported action %q rejected: %v", action, err)
		}
	}

	base.Action = "run_shell"
	err := ValidateCommand(base, "node-1", now)
	validationErr, ok := err.(*ValidationError)
	if !ok || validationErr.Code != "unsupported_action" {
		t.Fatalf("unknown action error = %#v, want unsupported_action", err)
	}
}

func TestDecodeRejectsUnknownEnvelopeAndParametersFields(t *testing.T) {
	envelopeJSON := `{"protocol_version":1,"message_type":"command","message_id":"m1","sent_at":"2026-08-23T00:00:00Z","payload":{},"unexpected":true}`
	if _, err := DecodeEnvelope([]byte(envelopeJSON)); err == nil {
		t.Fatal("DecodeEnvelope accepted an unknown field")
	}

	now := time.Now().UTC()
	command := Command{
		RequestID:  "request-2",
		NodeID:     "node-1",
		Action:     "change_ip",
		IssuedAt:   now.Add(-time.Second),
		DeadlineAt: now.Add(time.Minute),
		Parameters: json.RawMessage(`{"max_attempts":1,"shell":"rejected"}`),
	}
	err := ValidateCommand(command, "node-1", now)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown parameter field error = %v", err)
	}
}

func TestDecodeChangeIPParameterBounds(t *testing.T) {
	if _, err := DecodeChangeIPParameters([]byte(`{"max_attempts":4}`)); err == nil {
		t.Fatal("accepted max_attempts above the limit")
	}
	if _, err := DecodeChangeIPParameters([]byte(`{"timeout_seconds":29}`)); err == nil {
		t.Fatal("accepted timeout_seconds below the limit")
	}
}
