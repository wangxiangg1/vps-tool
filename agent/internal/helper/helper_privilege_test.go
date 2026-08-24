package helper

import (
	"reflect"
	"testing"
)

func TestPrivilegeEscalatorArguments(t *testing.T) {
	tests := []struct {
		name string
		tool privilegeEscalator
		want []string
	}{
		{
			name: "sudo",
			tool: privilegeEscalator{path: "/usr/bin/sudo"},
			want: []string{"-n", "--", "/usr/local/libexec/vps-agent-helper"},
		},
		{
			name: "doas",
			tool: privilegeEscalator{path: "/usr/bin/doas", doas: true},
			want: []string{"-n", "/usr/local/libexec/vps-agent-helper"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.tool.args("/usr/local/libexec/vps-agent-helper"); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("args() = %q; want %q", got, test.want)
			}
		})
	}
}
