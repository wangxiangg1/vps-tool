package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vps-tool/agent/internal/systemd"
)

type serviceManager string

const (
	managerSystemd serviceManager = "systemd"
	managerOpenRC  serviceManager = "openrc"
)

func currentServiceManager() (serviceManager, error) {
	if _, err := findProgram("systemctl"); err == nil && directoryExists("/run/systemd/system") {
		return managerSystemd, nil
	}
	if _, err := findProgram("rc-service"); err == nil && directoryExists("/etc/init.d") {
		return managerOpenRC, nil
	}
	return "", fmt.Errorf("no supported service manager was detected")
}

// serviceUnitState returns a manager-neutral state for both systemd and OpenRC.
func serviceUnitState(ctx context.Context, raw string) (string, error) {
	unit, err := systemd.NormalizeUnit(raw)
	if err != nil {
		return "", err
	}
	manager, err := currentServiceManager()
	if err != nil {
		return "", err
	}
	if manager == managerSystemd {
		result, runErr := runProgram(ctx, "systemctl", "is-active", unit)
		if runErr != nil {
			return "", runErr
		}
		switch firstLine(result.stdout) {
		case "active":
			return "active", nil
		case "inactive", "dead":
			return "inactive", nil
		case "failed":
			return "failed", nil
		case "unknown", "not-found":
			return "not-found", nil
		default:
			return "unknown", nil
		}
	}

	name := strings.TrimSuffix(unit, ".service")
	result, runErr := runProgram(ctx, "rc-service", name, "status")
	if runErr != nil {
		return "", runErr
	}
	output := strings.ToLower(strings.TrimSpace(string(append(result.stdout, result.stderr...))))
	return openRCStateFromOutput(output, result.exitCode), nil
}

func openRCStateFromOutput(output string, exitCode int) string {
	switch {
	case strings.Contains(output, "does not exist"), strings.Contains(output, "not found"):
		return "not-found"
	case strings.Contains(output, "started"), strings.Contains(output, "running"):
		return "active"
	case strings.Contains(output, "crashed"):
		return "failed"
	case strings.Contains(output, "stopped"), strings.Contains(output, "inactive"):
		return "inactive"
	case exitCode == 0:
		return "active"
	default:
		return "unknown"
	}
}

func serviceUnitStateForWarp(ctx context.Context, raw string) (string, error) {
	state, err := serviceUnitState(ctx, raw)
	if err != nil {
		return "", err
	}
	return warpStateFromService(state)
}

func warpStateFromService(state string) (string, error) {
	switch state {
	case "active":
		return "on", nil
	case "inactive":
		return "off", nil
	case "failed":
		return "degraded", nil
	case "not-found":
		return "unknown", nil
	default:
		return "", fmt.Errorf("service manager returned an unrecognized state %q", state)
	}
}

func serviceUnitStateForXUI(ctx context.Context, raw string) (string, error) {
	state, err := serviceUnitState(ctx, raw)
	if err != nil {
		return "", err
	}
	return xuiStateFromService(state), nil
}

func xuiStateFromService(state string) string {
	switch state {
	case "active":
		return "running"
	case "inactive":
		return "stopped"
	case "failed":
		return "failed"
	case "not-found":
		return "not_found"
	default:
		return "unknown"
	}
}

func serviceAction(ctx context.Context, raw, action string) error {
	unit, err := systemd.NormalizeUnit(raw)
	if err != nil {
		return err
	}
	manager, err := currentServiceManager()
	if err != nil {
		return err
	}
	name := unit
	program := "systemctl"
	args := []string{action, unit}
	if manager == managerOpenRC {
		program = "rc-service"
		name = strings.TrimSuffix(unit, ".service")
		args = []string{name, action}
	}
	result, runErr := runProgram(ctx, program, args...)
	if runErr != nil {
		return runErr
	}
	return requireSuccess(result, program+" "+action+" "+name)
}

func serviceExists(raw string) bool {
	unit, err := systemd.NormalizeUnit(raw)
	if err != nil {
		return false
	}
	manager, err := currentServiceManager()
	if err != nil {
		return false
	}
	if manager == managerOpenRC {
		return fileExists(filepath.Join("/etc/init.d", strings.TrimSuffix(unit, ".service")))
	}
	for _, directory := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
		if fileExists(filepath.Join(directory, unit)) {
			return true
		}
	}
	return false
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
