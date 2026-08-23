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
)

var supportedAdapters = map[string]struct{}{
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
	Path    string
	Adapter string
	Unit    string
	Timeout time.Duration
	DryRun  bool
	dry     *DryRun
	dryMu   sync.Mutex
}

type DryRun struct {
	AdapterState model.WarpSnapshot
	XUIState     model.XUISnapshot
	IPs          []string
	IPIndex      int
}

func NewRunner(path, adapter, unit string, dryRun bool) (*Runner, error) {
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
		Path:    path,
		Adapter: adapter,
		Unit:    unit,
		Timeout: defaultHelperTimeout,
		DryRun:  dryRun,
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
	if r.DryRun {
		r.dryMu.Lock()
		defer r.dryMu.Unlock()
		if len(r.dry.IPs) == 0 {
			return "", &Error{Code: "helper_failed", Message: "dry-run IP sequence is empty"}
		}
		ip := r.dry.IPs[r.dry.IPIndex%len(r.dry.IPs)]
		r.dry.IPIndex++
		return ip, nil
	}
	var response struct {
		IPv4 string `json:"ipv4"`
	}
	if err := r.callJSON(ctx, []string{"ip", r.Adapter}, &response); err != nil {
		return "", err
	}
	if response.IPv4 == "" {
		return "", &Error{Code: "helper_failed", Message: "helper returned an empty exit IP"}
	}
	return response.IPv4, nil
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

type xuiResponse struct {
	State string `json:"state"`
}

func (r *Runner) callJSON(ctx context.Context, args []string, target any) error {
	data, err := r.callOutput(ctx, args)
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
	if r.DryRun {
		return []byte(`{"state":"running"}`), nil
	}
	if r.Path == "" {
		return nil, &Error{Code: "helper_not_found", Message: "configured helper path is empty"}
	}
	if err := validateExecutable(r.Path); err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultHelperTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callCtx, r.Path, args...)
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
