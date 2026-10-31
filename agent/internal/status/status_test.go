package status

import (
	"testing"
)

func TestIsCloudflareWarpExitIP(t *testing.T) {
	cases := []struct {
		ipv4 string
		ipv6 string
		want bool
	}{
		{"104.28.195.192", "", true},
		{"104.29.1.1", "", true},
		{"", "2a09:bac5:636f:3141::4e8:5e", true},
		{"1.1.1.1", "", false},
		{"8.8.8.8", "2001:4860:4860::8888", false},
	}
	for _, tc := range cases {
		got := isCloudflareWarpExitIP(tc.ipv4, tc.ipv6)
		if got != tc.want {
			t.Errorf("isCloudflareWarpExitIP(%q, %q) = %v, want %v", tc.ipv4, tc.ipv6, got, tc.want)
		}
	}
}
