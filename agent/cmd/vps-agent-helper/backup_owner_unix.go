//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// adoptBackupOwnership returns a root-created snapshot to the low-privilege
// Agent process that requested it. Sudo exposes numeric IDs; doas commonly
// exposes the invoking username instead.
func adoptBackupOwnership(path string) error {
	uid, gid, ok := invokingUserIDs()
	if !ok {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("set backup ownership: %w", err)
	}
	return nil
}

func invokingUserIDs() (int, int, bool) {
	if uid, uidOK := numericEnv("SUDO_UID"); uidOK {
		if gid, gidOK := numericEnv("SUDO_GID"); gidOK {
			return uid, gid, true
		}
	}
	for _, name := range []string{os.Getenv("DOAS_USER"), os.Getenv("SUDO_USER")} {
		if name == "" {
			continue
		}
		entry, err := user.Lookup(name)
		if err != nil {
			continue
		}
		uid, uidErr := strconv.Atoi(entry.Uid)
		gid, gidErr := strconv.Atoi(entry.Gid)
		if uidErr == nil && gidErr == nil {
			return uid, gid, true
		}
	}
	return 0, 0, false
}

func numericEnv(name string) (int, bool) {
	value := os.Getenv(name)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed >= 0
}
