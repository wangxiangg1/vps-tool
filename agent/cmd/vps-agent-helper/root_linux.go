//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("helper must run as root")
	}
	return nil
}

func trustedOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}
