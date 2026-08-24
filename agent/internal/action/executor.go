package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"vps-tool/agent/internal/journal"
	"vps-tool/agent/internal/model"
	"vps-tool/agent/internal/protocol"
	"vps-tool/agent/internal/status"
	"vps-tool/agent/internal/warp"
	"vps-tool/agent/internal/warpchange"
)

type Result struct {
	RequestID  string    `json:"request_id"`
	Action     string    `json:"action"`
	State      string    `json:"state"`
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	Result     any       `json:"result,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

type Installer interface {
	InstallWARP(context.Context) (model.WarpSnapshot, error)
	InstallXUI(context.Context) (model.XUISnapshot, error)
	BackupPath(string) (string, error)
	BackupXUI(context.Context, string) (map[string]any, error)
	RestoreXUI(context.Context, string) error
}

type BackupTransport interface {
	Upload(context.Context, string, string) (int64, string, error)
	Download(context.Context, string, string) (int64, string, error)
}

type credentialSink interface {
	SetCredential(string)
}

type Executor struct {
	nodeID    string
	manager   *warp.Manager
	collector *status.FullCollector
	upgrader  interface {
		Upgrade(context.Context) (model.UpgradeResult, error)
	}
	installer       Installer
	backupTransport BackupTransport
	journal         *journal.Journal
	stateMu         sync.Mutex
	activeMu        sync.Mutex
	active          map[string]struct{}
}

func (e *Executor) SetCredential(value string) {
	if sink, ok := e.backupTransport.(credentialSink); ok {
		sink.SetCredential(value)
	}
}

func NewExecutor(
	nodeID string,
	manager *warp.Manager,
	collector *status.FullCollector,
	upgrader interface {
		Upgrade(context.Context) (model.UpgradeResult, error)
	},
	requestJournal *journal.Journal,
	installer Installer,
	backupTransport BackupTransport,
) (*Executor, error) {
	if nodeID == "" || manager == nil || collector == nil || upgrader == nil || requestJournal == nil {
		return nil, fmt.Errorf("action executor dependencies are incomplete")
	}
	return &Executor{
		nodeID:          nodeID,
		manager:         manager,
		collector:       collector,
		upgrader:        upgrader,
		journal:         requestJournal,
		installer:       installer,
		backupTransport: backupTransport,
		active:          make(map[string]struct{}),
	}, nil
}

func (e *Executor) Execute(ctx context.Context, command protocol.Command) Result {
	if previous, ok := e.journal.Lookup(command.RequestID); ok {
		if previous.Action != command.Action {
			return Result{
				RequestID:  command.RequestID,
				Action:     command.Action,
				State:      "failed",
				Code:       "request_conflict",
				Message:    "request_id is already associated with a different action",
				FinishedAt: time.Now().UTC(),
			}
		}
		return fromJournal(previous)
	}
	if validationErr := protocol.ValidateCommand(command, e.nodeID, time.Now().UTC()); validationErr != nil {
		result := Result{
			RequestID:  command.RequestID,
			Action:     command.Action,
			State:      "failed",
			Code:       validationCode(validationErr),
			Message:    validationErr.Error(),
			FinishedAt: time.Now().UTC(),
		}
		if validationErr := e.persist(result); validationErr != nil {
			result.Code = "journal_failed"
			result.Message = "invalid request could not be persisted"
		}
		return result
	}
	if !e.claim(command.RequestID) {
		return Result{
			RequestID:  command.RequestID,
			Action:     command.Action,
			State:      "running",
			Code:       "request_in_progress",
			Message:    "request is already executing",
			FinishedAt: time.Now().UTC(),
		}
	}
	defer e.release(command.RequestID)
	executionContext := ctx
	if !command.DeadlineAt.IsZero() {
		var cancel context.CancelFunc
		executionContext, cancel = context.WithDeadline(ctx, command.DeadlineAt)
		defer cancel()
	}

	if protocol.IsStateChanging(command.Action) {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		if previous, ok := e.journal.Lookup(command.RequestID); ok {
			return fromJournal(previous)
		}
	}

	result := e.execute(executionContext, command)
	if err := e.persist(result); err != nil {
		result.State = "failed"
		result.Code = "journal_failed"
		result.Message = "action completed but terminal result could not be persisted"
	}
	return result
}

func (e *Executor) claim(requestID string) bool {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	if _, exists := e.active[requestID]; exists {
		return false
	}
	e.active[requestID] = struct{}{}
	return true
}

func (e *Executor) release(requestID string) {
	e.activeMu.Lock()
	delete(e.active, requestID)
	e.activeMu.Unlock()
}

func (e *Executor) Recent() []Result {
	entries := e.journal.Entries()
	results := make([]Result, 0, len(entries))
	for _, entry := range entries {
		results = append(results, fromJournal(entry))
	}
	return results
}

func (e *Executor) execute(ctx context.Context, command protocol.Command) Result {
	result := Result{
		RequestID:  command.RequestID,
		Action:     command.Action,
		State:      "failed",
		Code:       "helper_failed",
		FinishedAt: time.Now().UTC(),
	}
	var payload any
	var err error
	switch command.Action {
	case "get_status":
		payload, err = e.collector.Collect(ctx)
	case "get_ip":
		var ips model.IPSnapshot
		ips, err = e.manager.GetIPs(ctx)
		payload = ips
	case "warp_on":
		var snapshot model.WarpSnapshot
		var note string
		snapshot, note, err = e.manager.TurnOn(ctx)
		payload = map[string]any{"warp": snapshot, "note": note}
	case "warp_off":
		var snapshot model.WarpSnapshot
		var note string
		snapshot, note, err = e.manager.TurnOff(ctx)
		payload = map[string]any{"warp": snapshot, "note": note}
	case "change_ip":
		var params protocol.ChangeIPParameters
		params, err = protocol.DecodeChangeIPParameters(command.Parameters)
		if err == nil {
			options := warpchange.Options{}
			if params.MaxAttempts != nil {
				options.MaxAttempts = *params.MaxAttempts
			}
			if params.TimeoutSeconds != nil {
				options.Timeout = time.Duration(*params.TimeoutSeconds) * time.Second
			}
			changeResult := e.manager.ChangeIP(ctx, options)
			payload = changeResult
			if changeResult.Success {
				result.State = "succeeded"
				result.Code = "ok"
				result.Message = changeResult.Message
				result.Result = payload
				result.FinishedAt = time.Now().UTC()
				return result
			}
			result.Code = changeResult.Code
			result.Message = changeResult.Message
		}
	case "restart_xui":
		var before, after model.XUISnapshot
		before, after, err = e.manager.RestartXUI(ctx)
		payload = map[string]any{"before": before, "after": after}
	case "upgrade_agent":
		payload, err = e.upgrader.Upgrade(ctx)
	case "install_warp":
		if e.installer == nil {
			err = &executorError{code: "unsupported_action", message: "WARP installer is not available"}
			break
		}
		payload, err = e.installer.InstallWARP(ctx)
	case "install_xui":
		if e.installer == nil {
			err = &executorError{code: "unsupported_action", message: "x-ui installer is not available"}
			break
		}
		payload, err = e.installer.InstallXUI(ctx)
	case "backup_xui":
		payload, err = e.backupXUI(ctx, command.Parameters)
	case "restore_xui":
		payload, err = e.restoreXUI(ctx, command.Parameters)
	default:
		err = &executorError{code: "unsupported_action", message: "action is not supported by this agent"}
	}
	if err != nil {
		result.Code = errorCode(err, result.Code)
		result.Message = err.Error()
		result.Result = payload
		result.FinishedAt = time.Now().UTC()
		return result
	}
	result.State = "succeeded"
	result.Code = "ok"
	result.Message = "action completed"
	result.Result = payload
	result.FinishedAt = time.Now().UTC()
	return result
}

func (e *Executor) backupXUI(ctx context.Context, raw []byte) (map[string]any, error) {
	if e.installer == nil || e.backupTransport == nil {
		return nil, &executorError{code: "unsupported_action", message: "x-ui backup transport is not available"}
	}
	parameters, err := protocol.DecodeBackupParameters(raw)
	if err != nil {
		return nil, err
	}
	path, err := e.installer.BackupPath(parameters.BackupID)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	metadata, err := e.installer.BackupXUI(ctx, path)
	if err != nil {
		return nil, err
	}
	size, checksum, err := e.backupTransport.Upload(ctx, parameters.BackupID, path)
	if err != nil {
		return nil, err
	}
	metadata["backup_id"] = parameters.BackupID
	metadata["size_bytes"] = size
	metadata["sha256"] = checksum
	return metadata, nil
}

func (e *Executor) restoreXUI(ctx context.Context, raw []byte) (map[string]any, error) {
	if e.installer == nil || e.backupTransport == nil {
		return nil, &executorError{code: "unsupported_action", message: "x-ui restore transport is not available"}
	}
	parameters, err := protocol.DecodeBackupParameters(raw)
	if err != nil {
		return nil, err
	}
	path, err := e.installer.BackupPath(parameters.BackupID)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	size, checksum, err := e.backupTransport.Download(ctx, parameters.BackupID, path)
	if err != nil {
		return nil, err
	}
	if err := e.installer.RestoreXUI(ctx, path); err != nil {
		return nil, err
	}
	return map[string]any{"backup_id": parameters.BackupID, "size_bytes": size, "sha256": checksum}, nil
}

func (e *Executor) persist(result Result) error {
	data, err := json.Marshal(result.Result)
	if err != nil {
		data = []byte(`null`)
	}
	return e.journal.Record(journal.Entry{
		RequestID:  result.RequestID,
		Action:     result.Action,
		State:      result.State,
		Code:       result.Code,
		Message:    result.Message,
		Result:     data,
		FinishedAt: result.FinishedAt,
	})
}

func fromJournal(entry journal.Entry) Result {
	var payload any
	if len(entry.Result) > 0 {
		_ = json.Unmarshal(entry.Result, &payload)
	}
	return Result{
		RequestID:  entry.RequestID,
		Action:     entry.Action,
		State:      entry.State,
		Code:       entry.Code,
		Message:    entry.Message,
		Result:     payload,
		FinishedAt: entry.FinishedAt,
	}
}

type executorError struct {
	code    string
	message string
}

func (e *executorError) Error() string     { return e.message }
func (e *executorError) ErrorCode() string { return e.code }

func errorCode(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) && coded.ErrorCode() != "" {
		return coded.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "action_timeout"
	}
	return fallback
}

func validationCode(err error) string {
	var validationErr *protocol.ValidationError
	if errors.As(err, &validationErr) && validationErr.Code != "" {
		return validationErr.Code
	}
	return "invalid_parameters"
}
