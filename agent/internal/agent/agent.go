package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"vps-tool/agent/internal/action"
	"vps-tool/agent/internal/backoff"
	"vps-tool/agent/internal/config"
	"vps-tool/agent/internal/protocol"
	"vps-tool/agent/internal/status"
	"vps-tool/agent/internal/wsclient"
)

const (
	heartbeatInterval = 30 * time.Second
	statusInterval    = 60 * time.Second
	writeTimeout      = 5 * time.Second
	statusTimeout     = 20 * time.Second
)

type Dialer interface {
	Dial(context.Context, string, http.Header) (wsclient.Conn, error)
}

type Agent struct {
	cfg       config.Config
	executor  *action.Executor
	collector *status.FullCollector
	dialer    Dialer
	logger    *log.Logger

	heartbeatEvery time.Duration
	statusEvery    time.Duration
	statusTimeout  time.Duration
}

func New(cfg config.Config, executor *action.Executor, collector *status.FullCollector, dialer Dialer, logger *log.Logger) (*Agent, error) {
	if executor == nil || collector == nil {
		return nil, fmt.Errorf("agent dependencies are incomplete")
	}
	if dialer == nil {
		dialer = &wsclient.Dialer{}
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Agent{
		cfg:            cfg,
		executor:       executor,
		collector:      collector,
		dialer:         dialer,
		logger:         logger,
		heartbeatEvery: heartbeatInterval,
		statusEvery:    statusInterval,
		statusTimeout:  statusTimeout,
	}, nil
}

func (a *Agent) Run(ctx context.Context) error {
	policy := backoff.New(time.Now().UnixNano())
	for {
		if ctx.Err() != nil {
			return nil
		}
		connection, err := a.dial(ctx)
		if err != nil {
			a.logger.Printf("WSS connection failed: %v", err)
			if err := wait(ctx, policy.Next()); err != nil {
				return nil
			}
			continue
		}
		policy.Reset()
		a.logger.Printf("WSS connection established")
		err = a.session(ctx, connection)
		_ = connection.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			a.logger.Printf("WSS connection closed: %v", err)
		}
		if err := wait(ctx, policy.Next()); err != nil {
			return nil
		}
	}
}

func (a *Agent) dial(ctx context.Context) (wsclient.Conn, error) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+a.cfg.Credential)
	return a.dialer.Dial(ctx, a.cfg.WSSURL, headers)
}

func (a *Agent) session(parent context.Context, connection wsclient.Conn) error {
	sessionCtx, cancel := context.WithCancel(parent)
	defer cancel()

	writeMu := &sync.Mutex{}
	send := func(ctx context.Context, messageType string, payload any) error {
		return a.send(ctx, connection, writeMu, messageType, payload)
	}
	if err := send(sessionCtx, protocol.MessageAgentHello, map[string]any{
		"node_id":          a.cfg.NodeID,
		"agent_version":    a.cfg.AgentVersion,
		"protocol_version": protocol.Version,
		"capabilities":     protocol.Actions(),
	}); err != nil {
		return err
	}
	if err := a.sendStatus(sessionCtx, send); err != nil {
		return err
	}
	for _, result := range a.executor.Recent() {
		if err := send(sessionCtx, protocol.MessageCommandResult, result); err != nil {
			return err
		}
	}

	readCh := make(chan []byte, 4)
	readErrCh := make(chan error, 1)
	go func() {
		for {
			data, err := connection.ReadMessage(sessionCtx)
			if err != nil {
				readErrCh <- err
				return
			}
			select {
			case readCh <- data:
			case <-sessionCtx.Done():
				return
			}
		}
	}()

	heartbeatTicker := time.NewTicker(a.heartbeatEvery)
	defer heartbeatTicker.Stop()
	statusTicker := time.NewTicker(a.statusEvery)
	defer statusTicker.Stop()
	statusCh := make(chan statusEvent, 1)
	statusRunning := false
	startStatus := func() {
		if statusRunning {
			return
		}
		statusRunning = true
		go func() {
			statusCtx, statusCancel := context.WithTimeout(sessionCtx, a.statusTimeout)
			report, err := a.collector.Collect(statusCtx)
			statusCancel()
			statusCh <- statusEvent{report: report, err: err}
		}()
	}

	for {
		select {
		case <-parent.Done():
			_ = connection.Close()
			return nil
		case err := <-readErrCh:
			return err
		case data := <-readCh:
			a.handleMessage(sessionCtx, connection, writeMu, data)
		case <-heartbeatTicker.C:
			if err := a.sendHeartbeat(sessionCtx, send); err != nil {
				return err
			}
		case <-statusTicker.C:
			startStatus()
		case event := <-statusCh:
			statusRunning = false
			if sessionCtx.Err() != nil {
				return sessionCtx.Err()
			}
			if err := send(sessionCtx, protocol.MessageStatusReport, event.report); err != nil {
				return err
			}
			if event.err != nil {
				a.logger.Printf("status report is partial: %v", event.err)
			}
		}
	}
}

type statusEvent struct {
	report status.Report
	err    error
}

func (a *Agent) sendStatus(ctx context.Context, send func(context.Context, string, any) error) error {
	statusCtx, cancel := context.WithTimeout(ctx, a.statusTimeout)
	defer cancel()
	report, err := a.collector.Collect(statusCtx)
	if sendErr := send(ctx, protocol.MessageStatusReport, report); sendErr != nil {
		return sendErr
	}
	if err != nil {
		a.logger.Printf("initial status report is partial: %v", err)
	}
	return nil
}

func (a *Agent) sendHeartbeat(ctx context.Context, send func(context.Context, string, any) error) error {
	payload := map[string]any{"node_id": a.cfg.NodeID}
	if report, ok := a.collector.Last(); ok {
		payload["warp_state"] = report.WarpState
		payload["xui_state"] = report.XUIState
		payload["uptime_seconds"] = report.UptimeSeconds
		payload["status_collected_at"] = report.CollectedAt
	}
	return send(ctx, protocol.MessageHeartbeat, payload)
}

func (a *Agent) handleMessage(ctx context.Context, connection wsclient.Conn, writeMu *sync.Mutex, data []byte) {
	envelope, err := protocol.DecodeEnvelope(data)
	if err != nil {
		_ = a.sendNotice(ctx, connection, writeMu, protocolErrorCode(err), err.Error())
		return
	}
	if envelope.MessageType == protocol.MessageServerNotice {
		return
	}
	if envelope.MessageType != protocol.MessageCommand {
		_ = a.sendNotice(ctx, connection, writeMu, "invalid_message", "agent only accepts command messages")
		return
	}
	command, err := protocol.DecodeCommand(envelope)
	if err != nil {
		_ = a.sendNotice(ctx, connection, writeMu, "invalid_parameters", err.Error())
		return
	}
	ack := map[string]any{
		"request_id": command.RequestID,
		"node_id":    a.cfg.NodeID,
		"action":     command.Action,
		"state":      "accepted",
	}
	if err := a.send(ctx, connection, writeMu, protocol.MessageCommandAck, ack); err != nil {
		return
	}
	go func() {
		result := a.executor.Execute(context.Background(), command)
		resultCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		_ = a.send(resultCtx, connection, writeMu, protocol.MessageCommandResult, result)
	}()
}

func (a *Agent) sendNotice(ctx context.Context, connection wsclient.Conn, writeMu *sync.Mutex, code, message string) error {
	return a.send(ctx, connection, writeMu, protocol.MessageServerNotice, protocol.Notice{Code: code, Message: message})
}

func (a *Agent) send(ctx context.Context, connection wsclient.Conn, writeMu *sync.Mutex, messageType string, payload any) error {
	envelope, err := protocol.New(messageType, payload)
	if err != nil {
		return err
	}
	data, err := protocol.Marshal(envelope)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	writeMu.Lock()
	defer writeMu.Unlock()
	return connection.WriteMessage(writeCtx, data)
}

func protocolErrorCode(err error) string {
	if strings.Contains(err.Error(), "unsupported protocol version") {
		return "agent_version_incompatible"
	}
	return "invalid_message"
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
