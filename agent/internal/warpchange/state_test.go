package warpchange

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vps-tool/agent/internal/model"
)

type fakeOps struct {
	mu       sync.Mutex
	state    model.WarpState
	ipQueue  []string
	onErr    error
	offErr   error
	onCalls  int
	offCalls int
}

func (f *fakeOps) WarpStatus(context.Context) (model.WarpSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return model.WarpSnapshot{State: f.state}, nil
}

func (f *fakeOps) WarpOn(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCalls++
	if f.onErr != nil {
		return f.onErr
	}
	f.state = model.WarpOn
	return nil
}

func (f *fakeOps) WarpOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offCalls++
	if f.offErr != nil {
		return f.offErr
	}
	f.state = model.WarpOff
	return nil
}

func (f *fakeOps) GetIP(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ipQueue) == 0 {
		return "", errors.New("no fake IP available")
	}
	ip := f.ipQueue[0]
	f.ipQueue = f.ipQueue[1:]
	return ip, nil
}

type fakeWatchdog struct {
	mu          sync.Mutex
	armCalls    int
	disarmCalls int
}

func (f *fakeWatchdog) Arm(context.Context, time.Time, int) error {
	f.mu.Lock()
	f.armCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeWatchdog) Disarm(context.Context) error {
	f.mu.Lock()
	f.disarmCalls++
	f.mu.Unlock()
	return nil
}

func newTestMachine(ops *fakeOps, watchdog *fakeWatchdog) *Machine {
	return &Machine{
		Ops:          ops,
		Watchdog:     watchdog,
		PollInterval: time.Millisecond,
		StepTimeout:  time.Second,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}
}

func TestChangeIPRejectsWarpOff(t *testing.T) {
	ops := &fakeOps{state: model.WarpOff}
	watchdog := &fakeWatchdog{}
	result := newTestMachine(ops, watchdog).Run(context.Background(), Options{MaxAttempts: 1, Timeout: 30 * time.Second})
	if result.Code != "invalid_state" {
		t.Fatalf("change_ip code = %q, want invalid_state", result.Code)
	}
	if watchdog.armCalls != 0 || ops.offCalls != 0 {
		t.Fatalf("change_ip started from off: watchdog=%d off=%d", watchdog.armCalls, ops.offCalls)
	}
}

func TestChangeIPRetriesUnchangedIPAndLeavesWarpOn(t *testing.T) {
	ops := &fakeOps{state: model.WarpOn, ipQueue: []string{"old", "old", "old", "old"}}
	watchdog := &fakeWatchdog{}
	result := newTestMachine(ops, watchdog).Run(context.Background(), Options{MaxAttempts: 3, Timeout: 30 * time.Second})
	if result.Success || result.Code != "ip_unchanged" {
		t.Fatalf("change_ip result = %#v, want ip_unchanged failure", result)
	}
	if result.Attempts != 3 || result.FinalWarpState != model.WarpOn {
		t.Fatalf("change_ip attempts/state = %d/%q", result.Attempts, result.FinalWarpState)
	}
	if ops.offCalls != 3 || ops.onCalls != 3 {
		t.Fatalf("change_ip toggles = off:%d on:%d", ops.offCalls, ops.onCalls)
	}
	if watchdog.armCalls != 1 || watchdog.disarmCalls != 1 {
		t.Fatalf("watchdog calls = arm:%d disarm:%d", watchdog.armCalls, watchdog.disarmCalls)
	}
}

func TestChangeIPAttemptsRecoveryAfterStartFailures(t *testing.T) {
	ops := &fakeOps{state: model.WarpOn, ipQueue: []string{"old"}, onErr: errors.New("start failed")}
	watchdog := &fakeWatchdog{}
	result := newTestMachine(ops, watchdog).Run(context.Background(), Options{MaxAttempts: 2, Timeout: 30 * time.Second})
	if result.Success || result.Code != "warp_start_failed" {
		t.Fatalf("change_ip result = %#v, want warp_start_failed failure", result)
	}
	if result.FinalWarpState != model.WarpOff || result.RecoveryError == "" {
		t.Fatalf("recovery state/error = %q/%q", result.FinalWarpState, result.RecoveryError)
	}
	if ops.onCalls != 3 {
		t.Fatalf("on calls = %d, want two attempts plus recovery", ops.onCalls)
	}
}
