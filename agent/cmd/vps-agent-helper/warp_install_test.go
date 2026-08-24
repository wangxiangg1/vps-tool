package main

import "testing"

func TestWarpInstallInput(t *testing.T) {
	tests := []struct {
		name        string
		kernel, tun bool
		want        string
	}{
		{name: "single implementation", kernel: false, tun: false, want: "2\n1\n3\n"},
		{name: "kernel and wireguard-go", kernel: true, tun: true, want: "2\n1\n1\n3\n"},
	}
	for _, test := range tests {
		if got := warpInstallInputForAvailability(test.kernel, test.tun); got != test.want {
			t.Fatalf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}
