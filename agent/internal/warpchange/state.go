package warpchange

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vps-tool/agent/internal/model"
)

const (
	DefaultTimeout      = 180 * time.Second
	DefaultMaxAttempts  = 3
	DefaultPollInterval = 1 * time.Second
	DefaultStepTimeout  = 20 * time.Second
)

type Ops interface {
	WarpStatus(context.Context) (model.WarpSnapshot, error)
	WarpOn(context.Context) error
	WarpOff(context.Context) error
	GetIP(context.Context) (string, error)
}

type Watchdog interface {
	Arm(context.Context, time.Time, int) error
	Disarm(context.Context) error
}

type Result struct {
	Success        bool            `json:"success"`
	Code           string          `json:"code"`
	Message        string          `json:"message"`
	OldIP          string          `json:"old_ip,omitempty"`
	NewIP          string          `json:"new_ip,omitempty"`
	Attempts       int             `json:"attempts"`
	FinalWarpState model.WarpState `json:"final_warp_state"`
	RecoveryError  string          `json:"recovery_error,omitempty"`
	WatchdogError  string          `json:"watchdog_error,omitempty"`
	DurationMS     int64           `json:"duration_ms"`
}

type Options struct {
	MaxAttempts int
	Timeout     time.Duration
}

type Machine struct {
	Ops          Ops
	Watchdog     Watchdog
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	PollInterval time.Duration
	StepTimeout  time.Duration
}

type stageError struct {
	code string
	err  error
}

func (e *stageError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e *stageError) Unwrap() error { return e.err }

func (e *stageError) ErrorCode() string { return e.code }

func (m *Machine) Run(parent context.Context, options Options) Result {
	started := m.clock()
	result := Result{Code: "unknown", Message: "change_ip did not complete"}
	if m.Ops == nil || m.Watchdog == nil {
		return m.finish(result, started, errors.New("change_ip dependencies are incomplete"))
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = DefaultMaxAttempts
	}
	if options.Timeout == 0 {
		options.Timeout = DefaultTimeout
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > DefaultMaxAttempts || options.Timeout < 30*time.Second || options.Timeout > DefaultTimeout {
		result.Code = "invalid_parameters"
		result.Message = "change_ip options are outside the allowed range"
		return m.finish(result, started, nil)
	}
	runCtx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	deadline := started.Add(options.Timeout)
	precheck, err := m.withStep(runCtx, func(ctx context.Context) (model.WarpSnapshot, error) {
		return m.Ops.WarpStatus(ctx)
	})
	if err != nil {
		result.Code = codeOf(err, "precheck_failed")
		result.Message = "unable to read WARP state before change_ip"
		return m.finish(result, started, err)
	}
	if precheck.State != model.WarpOn && precheck.State != model.WarpDegraded {
		result.Code = "invalid_state"
		result.Message = "change_ip requires WARP to be on or degraded"
		result.FinalWarpState = precheck.State
		return m.finish(result, started, nil)
	}
	oldIP, err := m.withIP(runCtx)
	if err != nil || oldIP == "" {
		result.Code = codeOf(err, "old_ip_unavailable")
		result.Message = "unable to determine the old WARP exit IP"
		result.FinalWarpState = precheck.State
		return m.finish(result, started, err)
	}
	result.OldIP = oldIP
	if err := m.Watchdog.Arm(runCtx, deadline, options.MaxAttempts); err != nil {
		result.Code = codeOf(err, "precheck_failed")
		result.Message = "unable to arm the WARP recovery watchdog"
		result.FinalWarpState = precheck.State
		return m.finish(result, started, err)
	}
	watchdogArmed := true
	defer func() {
		if watchdogArmed {
			// A failed disarm intentionally leaves the finite watchdog active.
			_ = m.Watchdog.Disarm(context.Background())
		}
	}()

	var lastErr error
	for attempt := 1; attempt <= options.MaxAttempts; attempt++ {
		result.Attempts = attempt
		if runCtx.Err() != nil {
			lastErr = runCtx.Err()
			result.Code = "action_timeout"
			break
		}
		if err := m.step(runCtx, func(ctx context.Context) error { return m.Ops.WarpOff(ctx) }); err != nil {
			lastErr = &stageError{code: "warp_stop_failed", err: err}
			continue
		}
		if err := m.waitFor(runCtx, func(state model.WarpSnapshot) bool { return state.State == model.WarpOff }); err != nil {
			lastErr = &stageError{code: "warp_stop_failed", err: err}
			continue
		}
		if err := m.step(runCtx, func(ctx context.Context) error { return m.Ops.WarpOn(ctx) }); err != nil {
			lastErr = &stageError{code: "warp_start_failed", err: err}
			continue
		}
		if err := m.waitFor(runCtx, func(state model.WarpSnapshot) bool { return state.State == model.WarpOn }); err != nil {
			lastErr = &stageError{code: "network_not_recovered", err: err}
			continue
		}
		newIP, ipErr := m.withIP(runCtx)
		if ipErr != nil || newIP == "" {
			lastErr = &stageError{code: "ip_check_failed", err: ipErr}
			continue
		}
		result.NewIP = newIP
		if newIP == oldIP {
			lastErr = &stageError{code: "ip_unchanged", err: fmt.Errorf("exit IP did not change")}
			continue
		}
		final, finalErr := m.currentState(runCtx)
		result.FinalWarpState = final.State
		if finalErr != nil || final.State != model.WarpOn {
			lastErr = &stageError{code: "network_not_recovered", err: finalErr}
			continue
		}
		result.Success = true
		result.Code = "ok"
		result.Message = "WARP exit IP changed and WARP is on"
		if err := m.Watchdog.Disarm(runCtx); err != nil {
			result.Success = false
			result.Code = "watchdog_disarm_failed"
			result.Message = "WARP is on, but recovery watchdog could not be canceled"
			result.WatchdogError = err.Error()
			return m.finish(result, started, err)
		}
		watchdogArmed = false
		return m.finish(result, started, nil)
	}

	if result.Code == "unknown" {
		result.Code = codeOf(lastErr, "action_timeout")
	}
	result.Message = messageForCode(result.Code)
	final, finalErr := m.currentState(runCtx)
	if finalErr != nil || final.State != model.WarpOn {
		final, finalErr = m.recoverOn(runCtx)
	}
	result.FinalWarpState = final.State
	if finalErr != nil {
		result.RecoveryError = finalErr.Error()
	}
	if finalErr == nil {
		if err := m.Watchdog.Disarm(context.Background()); err == nil {
			watchdogArmed = false
		} else {
			result.WatchdogError = err.Error()
		}
	}
	return m.finish(result, started, lastErr)
}

func (m *Machine) step(ctx context.Context, operation func(context.Context) error) error {
	stepCtx, cancel := m.stepContext(ctx)
	defer cancel()
	return operation(stepCtx)
}

func (m *Machine) withIP(ctx context.Context) (string, error) {
	stepCtx, cancel := m.stepContext(ctx)
	defer cancel()
	return m.Ops.GetIP(stepCtx)
}

func (m *Machine) currentState(ctx context.Context) (model.WarpSnapshot, error) {
	stepCtx, cancel := m.stepContext(ctx)
	defer cancel()
	return m.Ops.WarpStatus(stepCtx)
}

func (m *Machine) waitFor(ctx context.Context, predicate func(model.WarpSnapshot) bool) error {
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		}
		state, err := m.currentState(ctx)
		if err == nil && predicate(state) {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if err := m.sleep(ctx, m.pollInterval()); err != nil {
			return err
		}
	}
}

func (m *Machine) recoverOn(ctx context.Context) (model.WarpSnapshot, error) {
	recoveryCtx, cancel := context.WithTimeout(context.Background(), m.stepTimeout())
	defer cancel()
	if state, err := m.currentState(recoveryCtx); err == nil && state.State == model.WarpOn {
		return state, nil
	}
	if err := m.Ops.WarpOn(recoveryCtx); err != nil {
		state, stateErr := m.currentState(context.Background())
		if stateErr != nil {
			return state, errors.Join(err, stateErr)
		}
		return state, err
	}
	if err := m.waitFor(recoveryCtx, func(state model.WarpSnapshot) bool { return state.State == model.WarpOn }); err != nil {
		state, stateErr := m.currentState(context.Background())
		if stateErr != nil {
			return state, errors.Join(err, stateErr)
		}
		return state, err
	}
	return m.currentState(context.Background())
}

func (m *Machine) withStep(ctx context.Context, operation func(context.Context) (model.WarpSnapshot, error)) (model.WarpSnapshot, error) {
	stepCtx, cancel := m.stepContext(ctx)
	defer cancel()
	return operation(stepCtx)
}

func (m *Machine) stepContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, m.stepTimeout())
}

func (m *Machine) sleep(ctx context.Context, duration time.Duration) error {
	if m.Sleep != nil {
		return m.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Machine) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return DefaultPollInterval
}

func (m *Machine) stepTimeout() time.Duration {
	if m.StepTimeout > 0 {
		return m.StepTimeout
	}
	return DefaultStepTimeout
}

func (m *Machine) clock() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Machine) finish(result Result, started time.Time, err error) Result {
	if result.Code == "unknown" && err != nil {
		result.Code = codeOf(err, "helper_failed")
	}
	if result.Message == "" {
		result.Message = messageForCode(result.Code)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func codeOf(err error, fallback string) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		if code := coded.ErrorCode(); code != "" {
			return code
		}
	}
	return fallback
}

func messageForCode(code string) string {
	switch code {
	case "precheck_failed":
		return "WARP precheck failed"
	case "old_ip_unavailable":
		return "old exit IP is unavailable"
	case "warp_stop_failed":
		return "WARP could not be stopped"
	case "warp_start_failed":
		return "WARP could not be started"
	case "network_not_recovered":
		return "network or WARP tunnel did not recover"
	case "ip_check_failed":
		return "exit IP check failed"
	case "ip_unchanged":
		return "exit IP did not change within the attempt limit"
	case "action_timeout":
		return "change_ip exceeded its time limit"
	default:
		return "change_ip failed"
	}
}
