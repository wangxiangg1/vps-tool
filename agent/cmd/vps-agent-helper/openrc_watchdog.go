package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const openRCWatchdogDir = "/run/vps-agent/watchdogs"

func (b broker) armOpenRCWatchdog(adapter, token string, attempts int, rawDeadline string) error {
	if err := os.MkdirAll(openRCWatchdogDir, 0700); err != nil {
		return fmt.Errorf("create OpenRC watchdog directory: %w", err)
	}
	if err := os.Chmod(openRCWatchdogDir, 0700); err != nil {
		return fmt.Errorf("protect OpenRC watchdog directory: %w", err)
	}
	pidPath := openRCWatchdogPIDPath(token)
	command := exec.Command(b.selfPath, "watchdog-run", adapter, token, strconv.Itoa(attempts), rawDeadline)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open watchdog stdio: %w", err)
	}
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull
	if err := command.Start(); err != nil {
		_ = devNull.Close()
		return fmt.Errorf("start OpenRC watchdog: %w", err)
	}

	file, err := os.OpenFile(pidPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = devNull.Close()
		return fmt.Errorf("record OpenRC watchdog: %w", err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", command.Process.Pid); err != nil {
		_ = file.Close()
		_ = os.Remove(pidPath)
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = devNull.Close()
		return fmt.Errorf("write OpenRC watchdog pid: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(pidPath)
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = devNull.Close()
		return fmt.Errorf("close OpenRC watchdog pid: %w", err)
	}
	go func() {
		_ = command.Wait()
		_ = devNull.Close()
		_ = os.Remove(pidPath)
	}()
	return nil
}

func disarmOpenRCWatchdog(token string) error {
	pidPath := openRCWatchdogPIDPath(token)
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read OpenRC watchdog pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidPath)
		return fmt.Errorf("invalid OpenRC watchdog pid")
	}
	if !watchdogProcessMatches(pid, token) {
		_ = os.Remove(pidPath)
		return fmt.Errorf("OpenRC watchdog process identity mismatch")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		return nil
	}
	if err := process.Signal(os.Kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop OpenRC watchdog: %w", err)
	}
	_ = os.Remove(pidPath)
	return nil
}

func removeOpenRCWatchdogPID(token string) {
	_ = os.Remove(openRCWatchdogPIDPath(token))
}

func openRCWatchdogPIDPath(token string) string {
	return filepath.Join(openRCWatchdogDir, token+".pid")
}

func watchdogProcessMatches(pid int, token string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	for index, value := range args {
		if value == "watchdog-run" && index+2 < len(args) && args[index+2] == token {
			return true
		}
	}
	return false
}
