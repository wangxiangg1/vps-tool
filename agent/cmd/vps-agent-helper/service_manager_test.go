package main

import "testing"

func TestServiceStateMappings(t *testing.T) {
	tests := []struct {
		state string
		warp  string
		xui   string
	}{
		{state: "active", warp: "on", xui: "running"},
		{state: "inactive", warp: "off", xui: "stopped"},
		{state: "failed", warp: "degraded", xui: "failed"},
		{state: "not-found", warp: "unknown", xui: "not_found"},
	}
	for _, test := range tests {
		if got, err := warpStateFromService(test.state); err != nil || got != test.warp {
			t.Errorf("warpStateFromService(%q) = %q, %v; want %q", test.state, got, err, test.warp)
		}
		if got := xuiStateFromService(test.state); got != test.xui {
			t.Errorf("xuiStateFromService(%q) = %q; want %q", test.state, got, test.xui)
		}
	}
}

func TestServiceStateMappingsRejectUnknownWarpState(t *testing.T) {
	if _, err := warpStateFromService("unknown"); err == nil {
		t.Fatal("unknown service state was accepted for WARP")
	}
	if got := xuiStateFromService("unknown"); got != "unknown" {
		t.Fatalf("xuiStateFromService(unknown) = %q", got)
	}
}

func TestOpenRCStateFromOutput(t *testing.T) {
	tests := []struct {
		output string
		exit   int
		want   string
	}{
		{output: "status: started", exit: 0, want: "active"},
		{output: "status: stopped", exit: 3, want: "inactive"},
		{output: "status: crashed", exit: 32, want: "failed"},
		{output: "service `x-ui` does not exist", exit: 1, want: "not-found"},
		{output: "", exit: 0, want: "active"},
		{output: "unexpected", exit: 1, want: "unknown"},
	}
	for _, test := range tests {
		if got := openRCStateFromOutput(test.output, test.exit); got != test.want {
			t.Errorf("openRCStateFromOutput(%q, %d) = %q; want %q", test.output, test.exit, got, test.want)
		}
	}
}
