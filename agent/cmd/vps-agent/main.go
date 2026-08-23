package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"vps-tool/agent/internal/action"
	"vps-tool/agent/internal/backoff"
	"vps-tool/agent/internal/config"
	"vps-tool/agent/internal/helper"
	"vps-tool/agent/internal/journal"
	"vps-tool/agent/internal/protocol"
	"vps-tool/agent/internal/status"
	"vps-tool/agent/internal/warp"
	"vps-tool/agent/internal/wsclient"
)

var version = config.DefaultVersion

const (
	heartbeatInterval = 30 * time.Second
	statusInterval    = 60 * time.Second
	journalMaxEntries = 256
	journalMaxBytes   = 512 * 1024
)

func main() {
	configPath := flag.String("config", os.Getenv("VPS_AGENT_CONFIG"), "path to the restricted JSON config")
	once := flag.Bool("once", false, "collect one local status report and exit")
	dryRun := flag.Bool("dry-run", false, "use the fake helper backend for local validation")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load agent config: %v", err)
	}
	if cfg.RegistrationToken != "" && *configPath == "" {
		log.Fatalf("registration_token requires -config so the issued credential can be persisted")
	}
	if *dryRun {
		cfg.DryRun = true
	}
	runner, err := helper.NewRunner(cfg.HelperPath, cfg.WarpAdapter, cfg.XUIUnit, cfg.DryRun)
	if err != nil {
		log.Fatalf("initialize helper: %v", err)
	}
	requestJournal, err := journal.Open(cfg.StatePath, journalMaxEntries, journalMaxBytes)
	if err != nil {
		log.Fatalf("open request journal: %v", err)
	}
	manager, err := warp.NewManager(runner, runner)
	if err != nil {
		log.Fatalf("initialize action manager: %v", err)
	}
	collector := status.NewFullCollector(cfg.NodeID, cfg.AgentVersion, manager)
	executor, err := action.NewExecutor(cfg.NodeID, manager, collector, requestJournal)
	if err != nil {
		log.Fatalf("initialize action executor: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		report, collectErr := collector.Collect(ctx)
		if collectErr != nil {
			log.Printf("collect status: %v", collectErr)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatalf("write status: %v", err)
		}
		return
	}

	policy := backoff.New(time.Now().UnixNano())
	for ctx.Err() == nil {
		err := runConnectionWithBackoff(ctx, &cfg, *configPath, executor, collector, policy)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			log.Printf("agent connection ended: %v", err)
		}
		delay := policy.Next()
		log.Printf("reconnecting in %s", delay.Round(time.Millisecond))
		if err := sleepContext(ctx, delay); err != nil {
			break
		}
	}
}

func runConnection(ctx context.Context, cfg config.Config, executor *action.Executor, collector *status.FullCollector) error {
	return runConnectionWithBackoff(ctx, &cfg, "", executor, collector, nil)
}

func runConnectionWithBackoff(ctx context.Context, cfg *config.Config, configPath string, executor *action.Executor, collector *status.FullCollector, policy *backoff.Policy) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, 20*time.Second)
	defer cancelDial()
	headers := make(http.Header)
	if cfg.Credential != "" {
		headers.Set("Authorization", "Bearer "+cfg.Credential)
	}
	conn, err := (&wsclient.Dialer{}).Dial(dialCtx, cfg.WSSURL, headers)
	if err != nil {
		return err
	}
	defer conn.Close()

	hello := map[string]any{
		"node_id":          cfg.NodeID,
		"agent_version":    cfg.AgentVersion,
		"protocol_version": protocol.Version,
		"hostname":         hostname(),
		"architecture":     runtime.GOARCH,
		"capabilities":     protocol.Actions(),
		"reconcile":        reconcilePayload(executor.Recent()),
	}
	if cfg.RegistrationToken != "" {
		hello["registration_token"] = cfg.RegistrationToken
	}
	if err := sendEnvelope(ctx, conn, protocol.MessageAgentHello, hello); err != nil {
		return fmt.Errorf("send agent hello: %w", err)
	}
	issuedCredential, err := waitForAuthentication(ctx, conn)
	if err != nil {
		return err
	}
	if cfg.RegistrationToken != "" {
		if issuedCredential == "" {
			return errors.New("registration succeeded without an issued credential")
		}
		if err := config.PersistCredential(configPath, issuedCredential); err != nil {
			return err
		}
		cfg.Credential = issuedCredential
		cfg.RegistrationToken = ""
	}
	if policy != nil {
		policy.Reset()
	}
	for _, result := range executor.Recent() {
		if err := sendEnvelope(ctx, conn, protocol.MessageCommandResult, map[string]any{
			"request_id":    result.RequestID,
			"success":       result.State == "succeeded",
			"result":        objectValue(result.Result),
			"error_code":    result.Code,
			"error_message": result.Message,
		}); err != nil {
			return fmt.Errorf("send reconciled result: %w", err)
		}
	}

	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 3)
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		errorsCh <- readLoop(connectionCtx, conn, *cfg, executor)
	}()
	go func() {
		defer group.Done()
		errorsCh <- heartbeatLoop(connectionCtx, conn, cfg.NodeID)
	}()
	go func() {
		defer group.Done()
		errorsCh <- statusLoop(connectionCtx, conn, cfg.NodeID, collector)
	}()

	err = <-errorsCh
	cancel()
	_ = conn.Close()
	group.Wait()
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func waitForAuthentication(ctx context.Context, conn wsclient.Conn) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := conn.ReadMessage(readCtx)
	if err != nil {
		return "", fmt.Errorf("read authentication response: %w", err)
	}
	envelope, err := protocol.DecodeEnvelope(raw)
	if err != nil {
		return "", fmt.Errorf("decode authentication response: %w", err)
	}
	if envelope.MessageType != protocol.MessageServerNotice {
		return "", fmt.Errorf("unexpected authentication response %q", envelope.MessageType)
	}
	var notice map[string]any
	if err := json.Unmarshal(envelope.Payload, &notice); err != nil {
		return "", fmt.Errorf("decode authentication notice: %w", err)
	}
	if code, _ := notice["error_code"].(string); code != "" {
		message, _ := notice["message"].(string)
		return "", fmt.Errorf("agent authentication rejected: %s: %s", code, message)
	}
	issuedCredential, _ := notice["registered_credential"].(string)
	return issuedCredential, nil
}

func readLoop(ctx context.Context, conn wsclient.Conn, cfg config.Config, executor *action.Executor) error {
	for {
		raw, err := conn.ReadMessage(ctx)
		if err != nil {
			return err
		}
		envelope, err := protocol.DecodeEnvelope(raw)
		if err != nil {
			return err
		}
		switch envelope.MessageType {
		case protocol.MessageServerNotice:
			continue
		case protocol.MessageCommand:
			command, err := protocol.DecodeCommand(envelope)
			if err != nil {
				return sendCommandRejection(ctx, conn, "", err, "invalid_parameters")
			}
			go handleCommand(ctx, conn, cfg, executor, command)
		default:
			return fmt.Errorf("unsupported server message type %q", envelope.MessageType)
		}
	}
}

func handleCommand(ctx context.Context, conn wsclient.Conn, cfg config.Config, executor *action.Executor, command protocol.Command) {
	if err := protocol.ValidateCommand(command, cfg.NodeID, time.Now().UTC()); err != nil {
		_ = sendCommandRejection(ctx, conn, command.RequestID, err, validationCode(err))
		return
	}
	if err := sendEnvelope(ctx, conn, protocol.MessageCommandAck, map[string]any{
		"request_id": command.RequestID,
		"accepted":   true,
	}); err != nil {
		return
	}
	// The action must finish locally even when this WSS session is lost. The
	// journal makes the terminal result available for reconciliation after the
	// next connection; only result delivery uses the current session context.
	result := executor.Execute(context.Background(), command)
	payload := map[string]any{
		"request_id": command.RequestID,
		"success":    result.State == "succeeded",
		"result":     objectValue(result.Result),
	}
	if result.State != "succeeded" {
		payload["error_code"] = result.Code
		payload["error_message"] = result.Message
	}
	_ = sendEnvelope(ctx, conn, protocol.MessageCommandResult, payload)
}

func sendCommandRejection(ctx context.Context, conn wsclient.Conn, requestID string, err error, code string) error {
	return sendEnvelope(ctx, conn, protocol.MessageCommandAck, map[string]any{
		"request_id":    requestID,
		"accepted":      false,
		"error_code":    code,
		"error_message": err.Error(),
	})
}

func heartbeatLoop(ctx context.Context, conn wsclient.Conn, nodeID string) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sendEnvelope(ctx, conn, protocol.MessageHeartbeat, map[string]any{"node_id": nodeID}); err != nil {
				return err
			}
		}
	}
}

func statusLoop(ctx context.Context, conn wsclient.Conn, nodeID string, collector *status.FullCollector) error {
	if err := sendStatus(ctx, conn, nodeID, collector); err != nil {
		return err
	}
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sendStatus(ctx, conn, nodeID, collector); err != nil {
				return err
			}
		}
	}
}

func sendStatus(ctx context.Context, conn wsclient.Conn, nodeID string, collector *status.FullCollector) error {
	report, _ := collector.Collect(ctx)
	return sendEnvelope(ctx, conn, protocol.MessageStatusReport, map[string]any{
		"node_id":            nodeID,
		"agent_version":      report.AgentVersion,
		"hostname":           report.Hostname,
		"os_name":            report.OSName,
		"os_version":         report.OSVersion,
		"architecture":       report.Architecture,
		"protocol_version":   protocol.Version,
		"cpu_percent":        report.CPUPercent,
		"memory_used_bytes":  report.MemoryUsedBytes,
		"memory_total_bytes": report.MemoryTotalBytes,
		"root_used_bytes":    report.RootUsedBytes,
		"root_total_bytes":   report.RootTotalBytes,
		"uptime_seconds":     report.UptimeSeconds,
		"warp_status":        string(report.WarpState),
		"xui_status":         string(report.XUIState),
		"public_ipv4":        report.EgressIPv4,
		"public_ipv6":        report.EgressIPv6,
		"observed_at":        report.CollectedAt,
	})
}

func sendEnvelope(ctx context.Context, conn wsclient.Conn, messageType string, payload any) error {
	envelope, err := protocol.New(messageType, payload)
	if err != nil {
		return err
	}
	data, err := protocol.Marshal(envelope)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.WriteMessage(writeCtx, data)
}

func reconcilePayload(results []action.Result) []map[string]any {
	values := make([]map[string]any, 0, len(results))
	for _, result := range results {
		values = append(values, map[string]any{
			"request_id":    result.RequestID,
			"success":       result.State == "succeeded",
			"error_code":    result.Code,
			"error_message": result.Message,
			"result":        objectValue(result.Result),
		})
	}
	return values
}

func objectValue(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return map[string]any{"value": value}
	}
	return object
}

func validationCode(err error) string {
	var validationErr *protocol.ValidationError
	if errors.As(err, &validationErr) && validationErr.Code != "" {
		return validationErr.Code
	}
	return "invalid_parameters"
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return value
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
