package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vps-tool/agent/internal/config"
)

var version = config.DefaultVersion

const (
	defaultAgentPath  = "/usr/local/bin/vps-agent"
	defaultHelperPath = "/usr/local/libexec/vps-agent-helper"
	maxCommandOutput  = 16 * 1024
	commandTimeout    = 20 * time.Second
	maxWatchdogDelay  = 15 * time.Minute
	maxWatchdogToken  = 64
	watchdogPrefix    = "vps-agent-watchdog-"
	warpGoUnit        = "warp-go.service"
)

var programPaths = map[string][]string{
	"ip": {
		"/usr/sbin/ip", "/sbin/ip", "/usr/bin/ip", "/bin/ip",
	},
	"systemctl": {
		"/usr/bin/systemctl", "/bin/systemctl",
	},
	"rc-service": {
		"/sbin/rc-service", "/usr/sbin/rc-service", "/usr/bin/rc-service", "/bin/rc-service",
	},
	"systemd-run": {
		"/usr/bin/systemd-run", "/bin/systemd-run",
	},
	"warp-cli": {
		"/usr/bin/warp-cli", "/bin/warp-cli", "/usr/local/bin/warp-cli",
	},
	"wg": {
		"/usr/bin/wg", "/bin/wg", "/usr/sbin/wg", "/sbin/wg",
	},
	"wg-quick": {
		"/usr/bin/wg-quick", "/bin/wg-quick", "/usr/local/bin/wg-quick",
	},
	"bash": {
		"/usr/bin/bash", "/bin/bash",
	},
	"cat": {
		"/usr/bin/cat", "/bin/cat",
	},
	"sqlite3": {
		"/usr/bin/sqlite3", "/bin/sqlite3", "/usr/local/bin/sqlite3",
	},
}

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

type commandFailure struct {
	program string
	result  commandResult
}

func (e *commandFailure) Error() string {
	summary := strings.TrimSpace(string(e.result.stderr))
	if summary == "" {
		summary = strings.TrimSpace(string(e.result.stdout))
	}
	if len(summary) > 512 {
		summary = summary[:512]
	}
	message := fmt.Sprintf("%s exited with status %d", e.program, e.result.exitCode)
	if summary != "" {
		message += ": " + summary
	}
	return message
}

type binaryMissing struct{ name string }

func (e *binaryMissing) Error() string { return e.name + " is not installed" }

type warpBackend interface {
	Status(context.Context) (string, error)
	On(context.Context) error
	Off(context.Context) error
}

type broker struct {
	selfPath string
}

func main() {
	if err := requireRoot(); err != nil {
		fatal(err)
	}
	if err := dispatch(context.Background(), os.Args[1:]); err != nil {
		fatal(err)
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("helper command is required")
	}
	b := broker{selfPath: defaultHelperPath}
	if executable, err := os.Executable(); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil && filepath.IsAbs(resolved) {
			b.selfPath = resolved
		}
	}

	switch args[0] {
	case "warp":
		if len(args) != 3 {
			return errors.New("usage: warp <adapter> <status|on|off|install>")
		}
		return b.warp(ctx, args[1], args[2])
	case "ip":
		if len(args) != 2 {
			return errors.New("usage: ip <adapter>")
		}
		ipv4, ipv4Err := publicIPv4(ctx)
		ipv6, ipv6Err := publicIPv6(ctx)
		if ipv4Err != nil && ipv6Err != nil {
			return fmt.Errorf("IPv4 and IPv6 endpoint requests failed: %v; %v", ipv4Err, ipv6Err)
		}
		return writeJSON(ipResponse{IPv4: ipv4, IPv6: ipv6})
	case "xui":
		if len(args) != 3 && len(args) != 4 {
			return errors.New("usage: xui <unit> <status|restart|install|backup|restore> [path]")
		}
		path := ""
		if len(args) == 4 {
			path = args[3]
		}
		return b.xui(ctx, args[1], args[2], path)
	case "upgrade":
		if len(args) != 2 || args[1] != "latest" {
			return errors.New("usage: upgrade latest")
		}
		return b.upgrade(ctx)
	case "upgrade-restart":
		if len(args) != 1 {
			return errors.New("usage: upgrade-restart")
		}
		return b.restartAfterUpgrade(ctx)
	case "watchdog":
		return b.watchdog(ctx, args[1:])
	case "watchdog-run":
		if len(args) != 4 && len(args) != 5 {
			return errors.New("usage: watchdog-run <adapter> <token> <max-attempts> [deadline-unix]")
		}
		deadline := ""
		if len(args) == 5 {
			deadline = args[4]
		}
		return b.watchdogRun(ctx, args[1], args[2], args[3], deadline)
	default:
		return fmt.Errorf("unsupported helper command %q", args[0])
	}
}

func (b broker) warp(ctx context.Context, adapter, operation string) error {
	if operation == "install" {
		if adapter != "generic" && adapter != "fixed-helper" {
			return errors.New("WARP installation only supports the generic adapter")
		}
		return b.installWARP(ctx)
	}
	backend, err := b.backend(adapter)
	if err != nil {
		return err
	}
	switch operation {
	case "status":
		state, err := backend.Status(ctx)
		if err != nil {
			return err
		}
		return writeJSON(warpResponse{State: state})
	case "on":
		return backend.On(ctx)
	case "off":
		return backend.Off(ctx)
	default:
		return fmt.Errorf("unsupported WARP operation %q", operation)
	}
}

func (b broker) backend(adapter string) (warpBackend, error) {
	switch adapter {
	case "generic", "fixed-helper":
		return b.autoBackend()
	case "warp-cli":
		return cliBackend{}, nil
	case "wgcf":
		return wgcfBackend{interfaceName: "wgcf"}, nil
	case "warp-go":
		return warpGoBackend{}, nil
	default:
		return nil, fmt.Errorf("unsupported WARP adapter %q", adapter)
	}
}

func (b broker) autoBackend() (warpBackend, error) {
	if _, err := findProgram("warp-cli"); err == nil {
		return cliBackend{}, nil
	}
	if _, err := findProgram("wg-quick"); err == nil {
		for _, interfaceName := range []string{"warp", "wgcf"} {
			if fileExists("/etc/wireguard/" + interfaceName + ".conf") {
				return wgcfBackend{interfaceName: interfaceName}, nil
			}
		}
	}
	if serviceExists(warpGoUnit) {
		return warpGoBackend{}, nil
	}
	return nil, errors.New("no supported WARP backend was detected")
}

type cliBackend struct{}

func (cliBackend) Status(ctx context.Context) (string, error) {
	result, err := runProgram(ctx, "warp-cli", "status")
	if err != nil {
		return "", err
	}
	text := strings.ToLower(string(append(result.stdout, result.stderr...)))
	switch {
	case strings.Contains(text, "disconnected"), strings.Contains(text, "disconnect"):
		return "off", nil
	case strings.Contains(text, "connecting"), strings.Contains(text, "reconnecting"):
		return "degraded", nil
	case strings.Contains(text, "connected"), strings.Contains(text, "warp=on"):
		return "on", nil
	default:
		return "", errors.New("warp-cli returned an unrecognized status")
	}
}

func (cliBackend) On(ctx context.Context) error {
	result, err := runProgram(ctx, "warp-cli", "connect")
	if err != nil {
		return err
	}
	return requireSuccess(result, "warp-cli connect")
}

func (cliBackend) Off(ctx context.Context) error {
	result, err := runProgram(ctx, "warp-cli", "disconnect")
	if err != nil {
		return err
	}
	return requireSuccess(result, "warp-cli disconnect")
}

type wgcfBackend struct {
	interfaceName string
}

func (b wgcfBackend) Status(ctx context.Context) (string, error) {
	result, err := runProgram(ctx, "ip", "link", "show", "dev", b.interfaceName)
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "off", nil
	}
	return "on", nil
}

func (b wgcfBackend) On(ctx context.Context) error {
	result, err := runProgram(ctx, "wg-quick", "up", b.interfaceName)
	if err != nil {
		return err
	}
	return requireSuccess(result, "wg-quick up")
}

func (b wgcfBackend) Off(ctx context.Context) error {
	result, err := runProgram(ctx, "wg-quick", "down", b.interfaceName)
	if err != nil {
		return err
	}
	return requireSuccess(result, "wg-quick down")
}

type warpGoBackend struct{}

func (warpGoBackend) Status(ctx context.Context) (string, error) {
	return serviceUnitStateForWarp(ctx, warpGoUnit)
}

func (warpGoBackend) On(ctx context.Context) error {
	return serviceAction(ctx, warpGoUnit, "start")
}

func (warpGoBackend) Off(ctx context.Context) error {
	return serviceAction(ctx, warpGoUnit, "stop")
}

func (b broker) xui(ctx context.Context, rawUnit, operation, path string) error {
	switch operation {
	case "status":
		state, err := serviceUnitStateForXUI(ctx, rawUnit)
		if err != nil {
			return err
		}
		return writeJSON(xuiResponse{State: state})
	case "restart":
		return serviceAction(ctx, rawUnit, "restart")
	case "install":
		if err := installXUI(ctx); err != nil {
			return err
		}
		return serviceAction(ctx, rawUnit, "start")
	case "backup", "restore":
		return b.xuiDatabase(ctx, rawUnit, operation, path)
	default:
		return fmt.Errorf("unsupported x-ui operation %q", operation)
	}
}

const (
	warpInstallURL = "https://gitlab.com/fscarmen/warp/-/raw/main/menu.sh"
	xuiInstallURL  = "https://raw.githubusercontent.com/MHSanaei/3x-ui/master/install.sh"
	maxScriptBytes = 1024 * 1024
)

func (b broker) installWARP(ctx context.Context) error {
	// fscarmen's d mode asks for language, optionally WireGuard implementation,
	// global routing, and IP priority. Match the optional prompt instead of
	// blindly feeding a fixed sequence that could select non-global mode.
	script, err := downloadFixedScript(ctx, warpInstallURL)
	if err != nil {
		return err
	}
	defer os.Remove(script)
	input, err := warpInstallInput(ctx)
	if err != nil {
		return err
	}
	result, err := runProgramWithInput(ctx, 15*time.Minute, input, "bash", script, "d")
	if err != nil {
		return err
	}
	if err := requireSuccess(result, "fscarmen WARP install"); err != nil {
		return err
	}
	backend, err := b.autoBackend()
	if err != nil {
		return err
	}
	state, err := backend.Status(ctx)
	if err != nil {
		return err
	}
	if state != "on" {
		return fmt.Errorf("WARP install completed but interface state is %s", state)
	}
	return writeJSON(warpResponse{State: state})
}

func warpInstallInput(ctx context.Context) (string, error) {
	// The upstream script treats a loaded kernel module plus a usable TUN
	// device as having both implementations available and asks one extra
	// question. A TUN device reports "bad state" when read, which is the same
	// probe used by that script.
	kernelAvailable := pathExists("/sys/module/wireguard")
	tunAvailable := false
	if pathExists("/dev/net/tun") {
		result, err := runProgramWithInput(ctx, time.Second, "", "cat", "/dev/net/tun")
		if err != nil {
			return "", fmt.Errorf("probe TUN device: %w", err)
		}
		tunAvailable = strings.Contains(strings.ToLower(string(result.stderr)), "bad state") ||
			strings.Contains(strings.ToLower(string(result.stdout)), "bad state")
	}
	return warpInstallInputForAvailability(kernelAvailable, tunAvailable), nil
}

func warpInstallInputForAvailability(kernelAvailable, tunAvailable bool) string {
	if kernelAvailable && tunAvailable {
		return "2\n1\n1\n3\n"
	}
	return "2\n1\n3\n"
}

func installXUI(ctx context.Context) error {
	script, err := downloadFixedScript(ctx, xuiInstallURL)
	if err != nil {
		return err
	}
	defer os.Remove(script)
	result, err := runProgramWithInputEnv(ctx, 15*time.Minute, "", []string{"XUI_NONINTERACTIVE=1"}, "bash", script)
	if err != nil {
		return err
	}
	return requireSuccess(result, "MHSanaei 3x-ui install")
}

func (b broker) xuiDatabase(ctx context.Context, rawUnit, operation, path string) error {
	if err := validateAgentBackupPath(path); err != nil {
		return err
	}
	database, err := findXUIDatabase()
	if err != nil {
		return err
	}
	if operation == "backup" {
		method, err := snapshotXUIDatabase(ctx, database, path)
		if err != nil {
			return err
		}
		return writeJSON(xuiBackupResponse{Method: method, Database: database})
	}
	return restoreXUIDatabase(ctx, rawUnit, database, path)
}

func validateAgentBackupPath(path string) error {
	clean := filepath.Clean(path)
	root := filepath.Clean("/var/lib/vps-agent/xui-backups")
	if !filepath.IsAbs(path) || clean == root || !strings.HasPrefix(clean, root+string(os.PathSeparator)) || filepath.Base(clean) != strings.TrimSuffix(filepath.Base(clean), ".db")+".db" {
		return errors.New("backup path is outside the fixed agent backup directory")
	}
	return nil
}

func findXUIDatabase() (string, error) {
	for _, path := range []string{"/etc/x-ui/x-ui.db", "/etc/v2-ui/v2-ui.db", "/usr/local/x-ui/x-ui.db"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", errors.New("MHSanaei 3x-ui SQLite database was not found")
}

func snapshotXUIDatabase(ctx context.Context, source, destination string) (string, error) {
	if _, err := findProgram("sqlite3"); err == nil {
		result, runErr := runProgramWithInput(ctx, commandTimeout, "", "sqlite3", source, ".backup "+destination)
		if runErr == nil && result.exitCode == 0 {
			_ = os.Chmod(destination, 0600)
			return "sqlite3_online_backup", nil
		}
	}
	if err := copyFile(source, destination); err != nil {
		return "", fmt.Errorf("copy x-ui database without stopping service: %w", err)
	}
	return "filesystem_snapshot", nil
}

func restoreXUIDatabase(ctx context.Context, rawUnit, source, replacement string) error {
	backup := source + ".before-vps-tool-" + strconv.FormatInt(time.Now().Unix(), 10)
	if err := copyFile(source, backup); err != nil {
		return fmt.Errorf("preserve current x-ui database: %w", err)
	}
	manager, err := currentServiceManager()
	if err != nil {
		return err
	}
	if err := serviceAction(ctx, rawUnit, "stop"); err != nil {
		return err
	}
	temporary := source + ".vps-tool-restore.part"
	if err := copyFile(replacement, temporary); err != nil {
		_ = serviceAction(ctx, rawUnit, "start")
		return fmt.Errorf("stage x-ui database: %w", err)
	}
	if err := os.Rename(temporary, source); err != nil {
		_ = os.Remove(temporary)
		_ = serviceAction(ctx, rawUnit, "start")
		return fmt.Errorf("replace x-ui database: %w", err)
	}
	if err := serviceAction(ctx, rawUnit, "start"); err != nil {
		_ = copyFile(backup, source)
		_ = serviceAction(ctx, rawUnit, "start")
		return fmt.Errorf("start x-ui after restore (%s): %w", manager, err)
	}
	_ = os.Remove(backup)
	return nil
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary := destination + ".part"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(destination, 0600)
}

func downloadFixedScript(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "gitlab.com" && parsed.Hostname() != "raw.githubusercontent.com") {
		return "", errors.New("fixed installer URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", fmt.Errorf("download fixed installer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fixed installer returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxScriptBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > maxScriptBytes {
		return "", errors.New("fixed installer size is invalid")
	}
	file, err := os.CreateTemp("", "vps-tool-installer-*.sh")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0700); err != nil {
		file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func runProgramWithInput(ctx context.Context, timeout time.Duration, input, name string, args ...string) (commandResult, error) {
	return runProgramWithInputEnv(ctx, timeout, input, nil, name, args...)
}

func runProgramWithInputEnv(ctx context.Context, timeout time.Duration, input string, environment []string, name string, args ...string) (commandResult, error) {
	path, err := findProgram(name)
	if err != nil {
		return commandResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(callCtx, path, args...)
	command.Stdin = bytes.NewBufferString(input)
	if len(environment) > 0 {
		command.Env = append(os.Environ(), environment...)
	}
	var stdout, stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if callCtx.Err() != nil {
		return commandResult{}, fmt.Errorf("%s timed out: %w", name, callCtx.Err())
	}
	if stdout.exceeded || stderr.exceeded {
		return commandResult{}, fmt.Errorf("%s output exceeded the limit", name)
	}
	result := commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: 0}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.exitCode = exitErr.ExitCode()
			return result, nil
		}
		return commandResult{}, fmt.Errorf("run %s: %w", name, err)
	}
	return result, nil
}

func (b broker) watchdog(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: watchdog <adapter> arm|disarm ...")
	}
	switch args[1] {
	case "arm":
		if len(args) != 5 {
			return errors.New("usage: watchdog <adapter> arm <token> <deadline-unix> <max-attempts>")
		}
		if _, err := b.backend(args[0]); err != nil {
			return err
		}
		return b.armWatchdog(ctx, args[0], args[2], args[3], args[4])
	case "disarm":
		if len(args) != 3 {
			return errors.New("usage: watchdog <adapter> disarm <token>")
		}
		return b.disarmWatchdog(ctx, args[2])
	default:
		return fmt.Errorf("unsupported watchdog operation %q", args[1])
	}
}

func (b broker) armWatchdog(ctx context.Context, adapter, rawToken, rawDeadline, rawAttempts string) error {
	token, attempts, err := validateWatchdogInputs(rawToken, rawDeadline, rawAttempts)
	if err != nil {
		return err
	}
	manager, err := currentServiceManager()
	if err != nil {
		return err
	}
	if manager == managerOpenRC {
		return b.armOpenRCWatchdog(adapter, token, attempts, rawDeadline)
	}
	deadline, _ := strconv.ParseInt(rawDeadline, 10, 64)
	delay := deadline - time.Now().Unix()
	unit := watchdogUnit(token)
	result, err := runProgram(ctx, "systemd-run",
		"--unit="+unit,
		"--on-active="+strconv.FormatInt(delay, 10)+"s",
		"--collect",
		"--quiet",
		"--",
		b.selfPath,
		"watchdog-run",
		adapter,
		token,
		strconv.Itoa(attempts),
	)
	if err != nil {
		return err
	}
	return requireSuccess(result, "systemd-run watchdog")
}

func (b broker) disarmWatchdog(ctx context.Context, rawToken string) error {
	token, err := validateToken(rawToken)
	if err != nil {
		return err
	}
	manager, err := currentServiceManager()
	if err != nil {
		return err
	}
	if manager == managerOpenRC {
		return disarmOpenRCWatchdog(token)
	}
	result, err := runProgram(ctx, "systemctl", "stop", watchdogUnit(token))
	if err != nil {
		return err
	}
	return requireSuccess(result, "systemctl stop watchdog")
}

func (b broker) watchdogRun(ctx context.Context, adapter, rawToken, rawAttempts, rawDeadline string) error {
	if _, err := validateToken(rawToken); err != nil {
		return err
	}
	defer removeOpenRCWatchdogPID(rawToken)
	if rawDeadline != "" {
		_, _, err := validateWatchdogInputs(rawToken, rawDeadline, rawAttempts)
		if err != nil {
			return err
		}
		deadline, _ := strconv.ParseInt(rawDeadline, 10, 64)
		if err := sleepContext(ctx, time.Until(time.Unix(deadline, 0))); err != nil {
			return err
		}
	}
	attempts, err := strconv.Atoi(rawAttempts)
	if err != nil || attempts < 1 || attempts > 3 {
		return errors.New("watchdog max-attempts must be between 1 and 3")
	}
	backend, err := b.backend(adapter)
	if err != nil {
		return err
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		stateCtx, cancel := context.WithTimeout(ctx, commandTimeout)
		state, stateErr := backend.Status(stateCtx)
		cancel()
		if stateErr == nil && state == "on" {
			return nil
		}
		onCtx, onCancel := context.WithTimeout(ctx, commandTimeout)
		onErr := backend.On(onCtx)
		onCancel()
		if onErr == nil {
			verifyCtx, verifyCancel := context.WithTimeout(ctx, commandTimeout)
			verified, verifyErr := backend.Status(verifyCtx)
			verifyCancel()
			if verifyErr == nil && verified == "on" {
				return nil
			}
		}
		if attempt < attempts {
			if err := sleepContext(ctx, 2*time.Second); err != nil {
				return err
			}
		}
	}
	return errors.New("watchdog could not restore WARP")
}

func validateWatchdogInputs(rawToken, rawDeadline, rawAttempts string) (string, int, error) {
	token, err := validateToken(rawToken)
	if err != nil {
		return "", 0, err
	}
	deadline, err := strconv.ParseInt(rawDeadline, 10, 64)
	if err != nil {
		return "", 0, errors.New("watchdog deadline must be a Unix timestamp")
	}
	delay := time.Unix(deadline, 0).Sub(time.Now())
	if delay <= 0 || delay > maxWatchdogDelay {
		return "", 0, errors.New("watchdog deadline is outside the allowed window")
	}
	attempts, err := strconv.Atoi(rawAttempts)
	if err != nil || attempts < 1 || attempts > 3 {
		return "", 0, errors.New("watchdog max-attempts must be between 1 and 3")
	}
	return token, attempts, nil
}

func validateToken(value string) (string, error) {
	if value == "" || len(value) > maxWatchdogToken {
		return "", errors.New("invalid watchdog token")
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return "", errors.New("invalid watchdog token")
		}
	}
	return value, nil
}

func watchdogUnit(token string) string { return watchdogPrefix + token + ".service" }

func systemdWarpState(ctx context.Context, unit string) (string, error) {
	result, err := runProgram(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return "", err
	}
	value := firstLine(result.stdout)
	switch value {
	case "active":
		return "on", nil
	case "inactive", "dead":
		return "off", nil
	case "failed", "activating", "deactivating", "reloading":
		return "degraded", nil
	case "unknown", "not-found":
		return "unknown", nil
	default:
		return "", fmt.Errorf("systemctl returned an unrecognized state for %s", unit)
	}
}

func systemdServiceState(ctx context.Context, unit string) (string, error) {
	result, err := runProgram(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return "", err
	}
	value := firstLine(result.stdout)
	switch value {
	case "active":
		return "running", nil
	case "inactive", "dead":
		return "stopped", nil
	case "failed":
		return "failed", nil
	case "unknown", "not-found":
		return "not_found", nil
	default:
		return "unknown", nil
	}
}

func serviceFileExists(unit string) bool {
	for _, directory := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
		if fileExists(filepath.Join(directory, unit)) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func publicIPv4(ctx context.Context) (string, error) {
	return publicIP(ctx, "https://api.ipify.org", true)
}

func publicIPv6(ctx context.Context) (string, error) {
	return publicIP(ctx, "https://api6.ipify.org", false)
}

func publicIP(ctx context.Context, endpoint string, wantIPv4 bool) (string, error) {
	transport := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
	}
	client := &http.Client{Timeout: 8 * time.Second, Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("IP endpoint request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IP endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 128))
	if err != nil {
		return "", err
	}
	address, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || (wantIPv4 && !address.Is4()) || (!wantIPv4 && !address.Is6()) {
		family := "IPv6"
		if wantIPv4 {
			family = "IPv4"
		}
		return "", fmt.Errorf("IP endpoint returned an invalid %s address", family)
	}
	return address.String(), nil
}

func runProgram(ctx context.Context, name string, args ...string) (commandResult, error) {
	path, err := findProgram(name)
	if err != nil {
		return commandResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(callCtx, path, args...)
	var stdout, stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if callCtx.Err() != nil {
		return commandResult{}, fmt.Errorf("%s timed out: %w", name, callCtx.Err())
	}
	if stdout.exceeded || stderr.exceeded {
		return commandResult{}, fmt.Errorf("%s output exceeded the limit", name)
	}
	result := commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: 0}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.exitCode = exitErr.ExitCode()
			return result, nil
		}
		return commandResult{}, fmt.Errorf("run %s: %w", name, err)
	}
	return result, nil
}

func requireSuccess(result commandResult, operation string) error {
	if result.exitCode == 0 {
		return nil
	}
	return &commandFailure{program: operation, result: result}
}

func findProgram(name string) (string, error) {
	for _, path := range programPaths[name] {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&022 != 0 || !trustedOwner(info) {
			continue
		}
		if info.Mode()&0111 == 0 {
			continue
		}
		return path, nil
	}
	return "", &binaryMissing{name: name}
}

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := maxCommandOutput - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, errors.New("output limit exceeded")
	}
	if len(data) > remaining {
		_, _ = b.Buffer.Write(data[:remaining])
		b.exceeded = true
		return remaining, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(data)
}

func (b *limitedBuffer) Bytes() []byte { return b.Buffer.Bytes() }

func firstLine(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value != "" {
			return value
		}
	}
	return ""
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type warpResponse struct {
	State string `json:"state"`
}

type xuiResponse struct {
	State string `json:"state"`
}

type xuiBackupResponse struct {
	Method   string `json:"method"`
	Database string `json:"database"`
}

type ipResponse struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

func writeJSON(value any) error {
	return json.NewEncoder(os.Stdout).Encode(value)
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "vps-agent-helper:", err)
	os.Exit(1)
}
