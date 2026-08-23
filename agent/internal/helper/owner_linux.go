//go:build linux

package helper

import (
	"fmt"
	"os"
	"syscall"
)

func validateOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return &Error{Code: "permission_denied", Message: fmt.Sprintf("configured helper %q is not root-owned", path)}
	}
	return nil
}
