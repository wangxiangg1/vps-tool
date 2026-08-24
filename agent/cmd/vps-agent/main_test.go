package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"vps-tool/agent/internal/action"
	"vps-tool/agent/internal/protocol"
)

type resultLoopConn struct {
	writes   chan []byte
	writeErr error
}

func (c *resultLoopConn) ReadMessage(context.Context) ([]byte, error) {
	return nil, io.EOF
}

func (c *resultLoopConn) WriteMessage(_ context.Context, payload []byte) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	c.writes <- append([]byte(nil), payload...)
	return nil
}

func (c *resultLoopConn) Close() error { return nil }

func TestResultLoopSendsQueuedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan action.Result, 1)
	resultCh <- action.Result{RequestID: "req-1", State: "succeeded", Code: "ok", Message: "done"}
	conn := &resultLoopConn{writes: make(chan []byte, 1)}
	done := make(chan error, 1)
	go func() { done <- resultLoop(ctx, conn, resultCh) }()

	select {
	case raw := <-conn.writes:
		envelope, err := protocol.DecodeEnvelope(raw)
		if err != nil {
			t.Fatalf("decode result envelope: %v", err)
		}
		if envelope.MessageType != protocol.MessageCommandResult {
			t.Fatalf("message type = %q, want command_result", envelope.MessageType)
		}
	case <-time.After(time.Second):
		t.Fatal("queued result was not sent")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("result loop error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("result loop did not stop")
	}
}

func TestResultLoopRequeuesWhenConnectionFails(t *testing.T) {
	result := action.Result{RequestID: "req-2", State: "failed", Code: "helper_failed", Message: "connection lost"}
	resultCh := make(chan action.Result, 1)
	resultCh <- result
	conn := &resultLoopConn{writes: make(chan []byte, 1), writeErr: errors.New("disconnected")}

	err := resultLoop(context.Background(), conn, resultCh)
	if err == nil {
		t.Fatal("result loop returned nil after send failure")
	}
	select {
	case got := <-resultCh:
		if got.RequestID != result.RequestID {
			t.Fatalf("requeued request_id = %q, want %q", got.RequestID, result.RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("failed result was not requeued")
	}
}
