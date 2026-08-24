package warp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"vps-tool/agent/internal/model"
	"vps-tool/agent/internal/warpchange"
)

type Backend interface {
	warpchange.Ops
	XUIStatus(context.Context) (model.XUISnapshot, error)
	RestartXUI(context.Context) error
}

type WatchdogFactory interface {
	NewWatchdog() warpchange.Watchdog
}

type Manager struct {
	backend      Backend
	watchdogs    WatchdogFactory
	pollInterval time.Duration
	stepTimeout  time.Duration
	ipTTL        time.Duration
	ipMu         sync.Mutex
	cachedIPs    model.IPSnapshot
	cachedAt     time.Time
}

func NewManager(backend Backend, watchdogs WatchdogFactory) (*Manager, error) {
	if backend == nil || watchdogs == nil {
		return nil, fmt.Errorf("warp manager dependencies are incomplete")
	}
	return &Manager{
		backend:      backend,
		watchdogs:    watchdogs,
		pollInterval: warpchange.DefaultPollInterval,
		stepTimeout:  warpchange.DefaultStepTimeout,
		ipTTL:        5 * time.Minute,
	}, nil
}

func (m *Manager) SetTimings(pollInterval, stepTimeout time.Duration) {
	if pollInterval > 0 {
		m.pollInterval = pollInterval
	}
	if stepTimeout > 0 {
		m.stepTimeout = stepTimeout
	}
}

func (m *Manager) GetIP(ctx context.Context) (string, error) {
	ips, err := m.GetIPs(ctx)
	if err != nil {
		return "", err
	}
	if ips.IPv4 == "" {
		return "", &OperationError{CodeValue: "ip_check_failed", Message: "helper returned an empty IPv4 exit IP"}
	}
	return ips.IPv4, nil
}

func (m *Manager) GetIPs(ctx context.Context) (model.IPSnapshot, error) {
	m.ipMu.Lock()
	if (m.cachedIPs.IPv4 != "" || m.cachedIPs.IPv6 != "") && time.Since(m.cachedAt) < m.ipTTL {
		ips := m.cachedIPs
		m.ipMu.Unlock()
		return ips, nil
	}
	m.ipMu.Unlock()
	ips, err := m.backend.GetIPs(ctx)
	if err != nil {
		return model.IPSnapshot{}, err
	}
	if ips.IPv4 == "" && ips.IPv6 == "" {
		return model.IPSnapshot{}, &OperationError{CodeValue: "ip_check_failed", Message: "helper returned empty exit IPs"}
	}
	m.ipMu.Lock()
	m.cachedIPs = ips
	m.cachedAt = time.Now()
	m.ipMu.Unlock()
	return ips, nil
}

func (m *Manager) WarpStatus(ctx context.Context) (model.WarpSnapshot, error) {
	return m.backend.WarpStatus(ctx)
}

func (m *Manager) WarpOn(ctx context.Context) error {
	return m.backend.WarpOn(ctx)
}

func (m *Manager) WarpOff(ctx context.Context) error {
	return m.backend.WarpOff(ctx)
}

func (m *Manager) TurnOn(ctx context.Context) (model.WarpSnapshot, string, error) {
	return m.toggle(ctx, true)
}

func (m *Manager) TurnOff(ctx context.Context) (model.WarpSnapshot, string, error) {
	return m.toggle(ctx, false)
}

func (m *Manager) ChangeIP(ctx context.Context, options warpchange.Options) warpchange.Result {
	defer m.invalidateIP()
	machine := &warpchange.Machine{
		Ops:          m.backend,
		Watchdog:     m.watchdogs.NewWatchdog(),
		PollInterval: m.pollInterval,
		StepTimeout:  m.stepTimeout,
	}
	return machine.Run(ctx, options)
}

func (m *Manager) XUIStatus(ctx context.Context) (model.XUISnapshot, error) {
	return m.backend.XUIStatus(ctx)
}

func (m *Manager) RestartXUI(ctx context.Context) (before, after model.XUISnapshot, err error) {
	before, err = m.backend.XUIStatus(ctx)
	if err != nil {
		return before, model.XUISnapshot{State: model.XUIUnknown}, err
	}
	if before.State == model.XUINotFound {
		return before, before, &OperationError{CodeValue: "xui_not_found", Message: "configured x-ui unit was not found"}
	}
	if err = m.backend.RestartXUI(ctx); err != nil {
		return before, model.XUISnapshot{State: model.XUIUnknown}, err
	}
	after, err = m.waitXUI(ctx, model.XUIRunning)
	if err != nil {
		return before, after, err
	}
	return before, after, nil
}

func (m *Manager) toggle(ctx context.Context, on bool) (model.WarpSnapshot, string, error) {
	initial, err := m.backend.WarpStatus(ctx)
	if err != nil {
		return model.WarpSnapshot{State: model.WarpUnknown}, "", err
	}
	if on && initial.State == model.WarpOn {
		return initial, "already_on", nil
	}
	if !on && initial.State == model.WarpOff {
		return initial, "already_off", nil
	}
	if on {
		err = m.backend.WarpOn(ctx)
	} else {
		err = m.backend.WarpOff(ctx)
	}
	if err != nil {
		return model.WarpSnapshot{State: model.WarpUnknown}, "", err
	}
	m.invalidateIP()
	desired := model.WarpOff
	if on {
		desired = model.WarpOn
	}
	final, err := m.waitWarp(ctx, desired)
	if err != nil {
		return final, "", err
	}
	return final, "ok", nil
}

func (m *Manager) invalidateIP() {
	m.ipMu.Lock()
	m.cachedIPs = model.IPSnapshot{}
	m.cachedAt = time.Time{}
	m.ipMu.Unlock()
}

func (m *Manager) waitWarp(ctx context.Context, desired model.WarpState) (model.WarpSnapshot, error) {
	var last model.WarpSnapshot
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return last, lastErr
			}
			return last, &OperationError{CodeValue: "action_timeout", Message: "timed out waiting for WARP state", Cause: ctx.Err()}
		}
		state, err := m.backend.WarpStatus(ctx)
		last = state
		if err == nil && state.State == desired {
			return state, nil
		}
		if err != nil {
			lastErr = err
		}
		if err := sleepContext(ctx, m.pollInterval); err != nil {
			return last, err
		}
	}
}

func (m *Manager) waitXUI(ctx context.Context, desired model.XUIState) (model.XUISnapshot, error) {
	var last model.XUISnapshot
	for {
		if ctx.Err() != nil {
			return last, &OperationError{CodeValue: "action_timeout", Message: "timed out waiting for x-ui state", Cause: ctx.Err()}
		}
		state, err := m.backend.XUIStatus(ctx)
		last = state
		if err == nil && state.State == desired {
			return state, nil
		}
		if state.State == model.XUINotFound {
			return state, &OperationError{CodeValue: "xui_not_found", Message: "configured x-ui unit was not found"}
		}
		if err != nil {
			return state, err
		}
		if err := sleepContext(ctx, m.pollInterval); err != nil {
			return last, err
		}
	}
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

type OperationError struct {
	CodeValue string
	Message   string
	Cause     error
}

func (e *OperationError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return e.Message + ": " + e.Cause.Error()
}

func (e *OperationError) Unwrap() error { return e.Cause }

func (e *OperationError) ErrorCode() string { return e.CodeValue }

func ErrorCode(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) && coded.ErrorCode() != "" {
		return coded.ErrorCode()
	}
	return fallback
}
