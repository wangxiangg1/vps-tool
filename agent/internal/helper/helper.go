package helper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vps-tool/agent/internal/model"
	"vps-tool/agent/internal/validation"
	"vps-tool/agent/internal/warpchange"
)

const (
	maxOutputBytes       = 16 * 1024
	defaultHelperTimeout = 20 * time.Second
	upgradeHelperTimeout = 150 * time.Second
)

var backupIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{36,64}$`)

var supportedAdapters = map[string]struct{}{
	"generic":      {},
	"fixed-helper": {},
	"wgcf":         {},
	"warp-cli":     {},
	"warp-go":      {},
	"dry-run":      {},
}

type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) ErrorCode() string { return e.Code }

type Runner struct {
	Path                   string
	Adapter                string
	Unit                   string
	StatePath              string
	Timeout                time.Duration
	DryRun                 bool
	UsePrivilegeEscalation bool
	dry                    *DryRun
	dryMu                  sync.Mutex
}

type DryRun struct {
	AdapterState model.WarpSnapshot
	XUIState     model.XUISnapshot
	IPs          []string
	IPIndex      int
}

func NewRunner(path, adapter, unit, statePath string, dryRun bool) (*Runner, error) {
	if err := validation.AdapterName(adapter); err != nil {
		return nil, err
	}
	if _, ok := supportedAdapters[adapter]; !ok {
		return nil, fmt.Errorf("unsupported warp adapter %q", adapter)
	}
	if err := validation.UnitName(unit); err != nil {
		return nil, err
	}
	if path != "" {
		if err := validation.AbsolutePath(path, "helper_path"); err != nil {
			return nil, err
		}
	}
	runner := &Runner{
		Path:                   path,
		Adapter:                adapter,
		Unit:                   unit,
		StatePath:              statePath,
		Timeout:                defaultHelperTimeout,
		DryRun:                 dryRun,
		UsePrivilegeEscalation: !dryRun,
	}
	if dryRun {
		runner.dry = &DryRun{
			AdapterState: model.WarpSnapshot{State: model.WarpOn},
			XUIState:     model.XUISnapshot{State: model.XUIRunning},
			IPs:          []string{"198.51.100.10", "198.51.100.11"},
		}
	}
	return runner, nil
}

func (r *Runner) BackupPath(backupID string) (string, error) {
	if !backupIDPattern.MatchString(backupID) {
		return "", fmt.Errorf("invalid backup id")
	}
	base := filepath.Join(filepath.Dir(r.StatePath), "xui-backups")
	if r.StatePath == "" {
		base = "/var/lib/vps-agent/xui-backups"
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	return filepath.Join(base, backupID+".db"), nil
}

func (r *Runner) WarpStatus(ctx context.Context) (model.WarpSnapshot, error) {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		return r.dry.AdapterState, nil
	}
	var response warpResponse
	if err := r.callJSON(ctx, []string{"warp", r.Adapter, "status"}, &response); err != nil {
		return model.WarpSnapshot{}, err
	}
	state := model.WarpState(response.State)
	if !state.Valid() {
		return model.WarpSnapshot{}, &Error{Code: "helper_failed", Message: "helper returned an invalid WARP state"}
	}
	return model.WarpSnapshot{State: state, IPv4: response.IPv4, IPv6: response.IPv6}, nil
}

func (r *Runner) WarpOn(ctx context.Context) error {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		r.dry.AdapterState.State = model.WarpOn
		return nil
	}
	return r.call(ctx, []string{"warp", r.Adapter, "on"})
}

func (r *Runner) WarpOff(ctx context.Context) error {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		r.dry.AdapterState.State = model.WarpOff
		return nil
	}
	return r.call(ctx, []string{"warp", r.Adapter, "off"})
}

func (r *Runner) GetIP(ctx context.Context) (string, error) {
	ips, err := r.GetIPs(ctx)
	if err != nil {
		return "", err
	}
	if ips.IPv4 == "" {
		return "", &Error{Code: "helper_failed", Message: "helper returned an empty IPv4 address"}
	}
	return ips.IPv4, nil
}

func (r *Runner) GetIPs(ctx context.Context) (model.IPSnapshot, error) {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		if len(r.dry.IPs) == 0 {
			return model.IPSnapshot{}, &Error{Code: "helper_failed", Message: "dry-run IP sequence is empty"}
		}
		ip := r.dry.IPs[r.dry.IPIndex%len(r.dry.IPs)]
		r.dry.IPIndex++
		return model.IPSnapshot{IPv4: ip}, nil
	}
	var response model.IPSnapshot
	if err := r.callJSON(ctx, []string{"ip", r.Adapter}, &response); err != nil {
		return model.IPSnapshot{}, err
	}
	if response.IPv4 == "" && response.IPv6 == "" {
		return model.IPSnapshot{}, &Error{Code: "helper_failed", Message: "helper returned empty exit IPs"}
	}
	return response, nil
}

func (r *Runner) XUIStatus(ctx context.Context) (model.XUISnapshot, error) {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		return r.dry.XUIState, nil
	}
	var response xuiResponse
	if err := r.callJSON(ctx, []string{"xui", r.Unit, "status"}, &response); err != nil {
		return model.XUISnapshot{}, err
	}
	state := model.XUIState(response.State)
	if !state.Valid() {
		return model.XUISnapshot{}, &Error{Code: "helper_failed", Message: "helper returned an invalid x-ui state"}
	}
	return model.XUISnapshot{State: state}, nil
}

func (r *Runner) RestartXUI(ctx context.Context) error {
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		r.dry.XUIState.State = model.XUIRunning
		return nil
	}
	return r.call(ctx, []string{"xui", r.Unit, "restart"})
}

func (r *Runner) Upgrade(ctx context.Context) (model.UpgradeResult, error) {
	if r.DryRun {
		return model.UpgradeResult{Version: "dry-run", Changed: false, RestartScheduled: false}, nil
	}
	var response model.UpgradeResult
	if err := r.callJSONWithTimeout(ctx, []string{"upgrade", "latest"}, &response, upgradeHelperTimeout); err != nil {
		return model.UpgradeResult{}, err
	}
	if response.Version == "" {
		return model.UpgradeResult{}, &Error{Code: "helper_failed", Message: "helper returned an empty release version"}
	}
	return response, nil
}

func (r *Runner) InstallWARP(ctx context.Context) (model.WarpSnapshot, error) {
	if r.DryRun {
		return model.WarpSnapshot{State: model.WarpOn, IPv4: "198.51.100.10", IPv6: "2001:db8::10"}, nil
	}
	var response warpResponse
	if err := r.callJSON(ctx, []string{"warp", "install"}, &response); err != nil {
		return model.WarpSnapshot{}, err
	}
	state := model.WarpState(response.State)
	if !state.Valid() {
		return model.WarpSnapshot{}, &Error{Code: "helper_failed", Message: "helper returned an invalid WARP state"}
	}
	return model.WarpSnapshot{State: state, IPv4: response.IPv4, IPv6: response.IPv6}, nil
}

func (r *Runner) InstallXUI(ctx context.Context) (model.XUISnapshot, error) {
	if r.DryRun {
		return model.XUISnapshot{State: model.XUIRunning}, nil
	}
	if err := r.call(ctx, []string{"xui", r.Unit, "install"}); err != nil {
		return model.XUISnapshot{}, err
	}
	return r.XUIStatus(ctx)
}

func (r *Runner) BackupXUI(ctx context.Context, path string) (map[string]any, error) {
	if r.DryRun {
		return map[string]any{"path": path, "method": "dry-run", "database": "/etc/x-ui/x-ui.db"}, nil
	}
	var response xuiBackupResponse
	if err := r.callJSON(ctx, []string{"xui", r.Unit, "backup", path}, &response); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "method": response.Method, "database": response.Database}, nil
}

func (r *Runner) RestoreXUI(ctx context.Context, path string) error {
	if r.DryRun {
		return nil
	}
	return r.call(ctx, []string{"xui", r.Unit, "restore", path})
}

func (r *Runner) NewWatchdog() warpchange.Watchdog {
	return &watchdog{runner: r}
}

type watchdog struct {
	runner *Runner
	token  string
}

func (w *watchdog) Arm(ctx context.Context, deadline time.Time, maxAttempts int) error {
	w.token = randomToken()
	if w.runner.DryRun {
		return nil
	}
	return w.runner.call(ctx, []string{
		"watchdog", w.runner.Adapter, "arm", w.token,
		strconv.FormatInt(deadline.Unix(), 10), strconv.Itoa(maxAttempts),
	})
}

func (w *watchdog) Disarm(ctx context.Context) error {
	if w.runner.DryRun {
		return nil
	}
	if w.token == "" {
		return nil
	}
	return w.runner.call(ctx, []string{"watchdog", w.runner.Adapter, "disarm", w.token})
}

type warpResponse struct {
	State string `json:"state"`
	IPv4  string `json:"ipv4,omitempty"`
	IPv6  string `json:"ipv6,omitempty"`
}

type xuiBackupResponse struct {
	Method   string `json:"method"`
	Database string `json:"database"`
}

type xuiResponse struct {
	State string `json:"state"`
}

func (r *Runner) callJSON(ctx context.Context, args []string, target any) error {
	return r.callJSONWithTimeout(ctx, args, target, r.Timeout)
}

func (r *Runner) callJSONWithTimeout(ctx context.Context, args []string, target any, timeout time.Duration) error {
	data, err := r.callOutputWithTimeout(ctx, args, timeout)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &Error{Code: "helper_failed", Message: "helper returned invalid JSON", Err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return &Error{Code: "helper_failed", Message: "helper returned trailing data"}
	}
	return nil
}

func (r *Runner) call(ctx context.Context, args []string) error {
	_, err := r.callOutput(ctx, args)
	return err
}

func (r *Runner) callOutput(ctx context.Context, args []string) ([]byte, error) {
	return r.callOutputWithTimeout(ctx, args, r.Timeout)
}

func (r *Runner) callOutputWithTimeout(ctx context.Context, args []string, timeout time.Duration) ([]byte, error) {
	if r.DryRun {
		return []byte(`{"state":"running"}`), nil
	}
	if r.Path == "" {
		return nil, &Error{Code: "helper_not_found", Message: "configured helper path is empty"}
	}
	if err := validateExecutable(r.Path); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultHelperTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandPath := r.Path
	commandArgs := args
	if r.UsePrivilegeEscalation {
		escalator, err := findPrivilegeEscalator()
		if err != nil {
			return nil, err
		}
		commandPath = escalator.path
		commandArgs = append(escalator.args(r.Path), args...)
	}
	command := exec.CommandContext(callCtx, commandPath, commandArgs...)
	command.Stdin = nil
	var stdout, stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if callCtx.Err() != nil {
		return nil, &Error{Code: "helper_failed", Message: "helper timed out", Err: callCtx.Err()}
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, &Error{Code: "helper_not_found", Message: "configured helper was not found", Err: err}
	}
	if err != nil {
		summary := strings.TrimSpace(stderr.String())
		if summary == "" {
			summary = strings.TrimSpace(stdout.String())
		}
		if len(summary) > 512 {
			summary = summary[:512]
		}
		message := "helper execution failed"
		if summary != "" {
			message += ": " + summary
		}
		return nil, &Error{Code: "helper_failed", Message: message, Err: err}
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, &Error{Code: "helper_failed", Message: "helper output exceeded the limit"}
	}
	return stdout.Bytes(), nil
}

type privilegeEscalator struct {
	path string
	doas bool
}

func (p privilegeEscalator) args(helperPath string) []string {
	if p.doas {
		return []string{"-n", helperPath}
	}
	return []string{"-n", "--", helperPath}
}

func findPrivilegeEscalator() (privilegeEscalator, error) {
	// The installer writes this root-owned rule only when it selected native
	// doas. Prefer doas in that case so a separately installed sudo shim cannot
	// change the command-line contract.
	if _, err := os.Stat("/etc/doas.d/vps-agent.conf"); err == nil {
		if escalator, ok := findEscalator([]string{"/usr/bin/doas", "/bin/doas"}, true); ok {
			return escalator, nil
		}
	}
	if escalator, ok := findEscalator([]string{"/usr/bin/sudo", "/bin/sudo"}, false); ok {
		return escalator, nil
	}
	if escalator, ok := findEscalator([]string{"/usr/bin/doas", "/bin/doas"}, true); ok {
		return escalator, nil
	}
	return privilegeEscalator{}, &Error{Code: "helper_not_found", Message: "sudo or doas was not found or is not safely installed"}
}

func findEscalator(candidates []string, doas bool) (privilegeEscalator, bool) {
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if err := validateExecutable(candidate); err == nil {
			return privilegeEscalator{path: candidate, doas: doas}, true
		}
	}
	return privilegeEscalator{}, false
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Error{Code: "helper_not_found", Message: "configured helper was not found", Err: err}
	}
	if err != nil {
		return &Error{Code: "helper_failed", Message: "cannot inspect configured helper", Err: err}
	}
	if !info.Mode().IsRegular() {
		return &Error{Code: "helper_failed", Message: "configured helper is not a regular file"}
	}
	if info.Mode().Perm()&022 != 0 {
		return &Error{Code: "permission_denied", Message: "configured helper is writable by group or others"}
	}
	if err := validateOwner(path, info); err != nil {
		return err
	}
	if info.Mode()&0111 == 0 && os.PathSeparator == '/' {
		return &Error{Code: "permission_denied", Message: "configured helper is not executable"}
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := maxOutputBytes - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, fmt.Errorf("output limit exceeded")
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.exceeded = true
		return remaining, fmt.Errorf("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.Buffer.Bytes() }

func (b *limitedBuffer) String() string { return b.Buffer.String() }

func randomToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(raw[:])
}
