package adapter

import (
	"context"
	"errors"
	"sync"

	"vps-tool/agent/internal/model"
)

// Fake is deliberately small so tests and dry-run mode can exercise the
// action paths without invoking a helper or a network request.
type FakeWarp struct {
	mu sync.Mutex

	WarpState model.WarpState

	StatusErr error
	OnErr     error
	OffErr    error

	OnCalls  int
	OffCalls int
}

func NewFakeWarp() *FakeWarp {
	return &FakeWarp{WarpState: model.WarpOn}
}

func (f *FakeWarp) Status(context.Context) (model.WarpState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return model.WarpUnknown, f.StatusErr
	}
	return f.WarpState, nil
}

func (f *FakeWarp) On(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.OnCalls++
	if f.OnErr != nil {
		return f.OnErr
	}
	f.WarpState = model.WarpOn
	return nil
}

func (f *FakeWarp) Off(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.OffCalls++
	if f.OffErr != nil {
		return f.OffErr
	}
	f.WarpState = model.WarpOff
	return nil
}

type FakeXUI struct {
	mu sync.Mutex

	XUIState     model.XUIState
	StatusErr    error
	RestartErr   error
	RestartCalls int
}

func NewFakeXUI() *FakeXUI {
	return &FakeXUI{XUIState: model.XUIRunning}
}

func (f *FakeXUI) Status(context.Context, string) (model.XUIState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return model.XUIUnknown, f.StatusErr
	}
	return f.XUIState, nil
}

func (f *FakeXUI) Restart(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RestartCalls++
	if f.RestartErr != nil {
		return f.RestartErr
	}
	f.XUIState = model.XUIRunning
	return nil
}

type FakeIP struct {
	mu        sync.Mutex
	IPv4Queue []string
	IPv6Queue []string
	IPv4Err   error
	IPv6Err   error
}

func (f *FakeIP) IPv4(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.IPv4Err != nil {
		return "", f.IPv4Err
	}
	if len(f.IPv4Queue) == 0 {
		return "", errors.New("fake IPv4 queue is empty")
	}
	ip := f.IPv4Queue[0]
	f.IPv4Queue = f.IPv4Queue[1:]
	return ip, nil
}

func (f *FakeIP) IPv6(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.IPv6Err != nil {
		return "", f.IPv6Err
	}
	if len(f.IPv6Queue) == 0 {
		return "", nil
	}
	ip := f.IPv6Queue[0]
	f.IPv6Queue = f.IPv6Queue[1:]
	return ip, nil
}
