package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	releaseAPIURL      = "https://api.github.com/repos/wangxiangg1/vps-tool/releases/latest"
	releaseDownloadURL = "https://github.com/wangxiangg1/vps-tool/releases/download"
	maxChecksumBytes   = 1024 * 1024
	maxBinaryBytes     = 32 * 1024 * 1024
	upgradeHTTPTimeout = 45 * time.Second
	upgradeRestartWait = 3 * time.Second
)

type latestRelease struct {
	TagName string `json:"tag_name"`
}

type upgradeResponse struct {
	Version          string `json:"version"`
	Changed          bool   `json:"changed"`
	RestartScheduled bool   `json:"restart_scheduled"`
}

type binaryUpdate struct {
	target  string
	temp    string
	backup  string
	changed bool
}

func (b broker) upgrade(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("Agent upgrades are only supported on Linux")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported Agent architecture %q", runtime.GOARCH)
	}

	tag, err := resolveLatestRelease(ctx)
	if err != nil {
		return err
	}
	latestVersion, _ := parseReleaseVersion(tag)
	currentVersion, currentErr := parseReleaseVersion("v" + version)
	if currentErr != nil {
		return fmt.Errorf("installed helper has invalid version %q", version)
	}
	if compareReleaseVersions(latestVersion, currentVersion) < 0 {
		return fmt.Errorf("latest release %s is older than installed version v%s", tag, version)
	}
	checksumsData, err := downloadReleaseAsset(ctx, tag, "SHA256SUMS", maxChecksumBytes)
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(checksumsData)
	if err != nil {
		return err
	}

	agentAsset := "vps-agent-linux-" + runtime.GOARCH
	helperAsset := "vps-agent-helper-linux-" + runtime.GOARCH
	agentData, err := downloadVerifiedAsset(ctx, tag, agentAsset, checksums)
	if err != nil {
		return err
	}
	helperData, err := downloadVerifiedAsset(ctx, tag, helperAsset, checksums)
	if err != nil {
		return err
	}

	updates := make([]binaryUpdate, 0, 2)
	for _, item := range []struct {
		target string
		data   []byte
	}{
		{target: defaultAgentPath, data: agentData},
		{target: defaultHelperPath, data: helperData},
	} {
		update, prepareErr := prepareBinaryUpdate(item.target, item.data)
		if prepareErr != nil {
			cleanupStagedUpdates(updates)
			return prepareErr
		}
		updates = append(updates, update)
	}
	defer cleanupStagedUpdates(updates)

	changed := false
	for _, update := range updates {
		changed = changed || update.changed
	}
	version := strings.TrimPrefix(tag, "v")
	if !changed {
		return writeJSON(upgradeResponse{Version: version})
	}
	if err := commitBinaryUpdates(updates); err != nil {
		return err
	}
	if err := b.scheduleUpgradeRestart(); err != nil {
		rollbackBinaryUpdates(updates)
		return fmt.Errorf("schedule Agent restart: %w", err)
	}
	return writeJSON(upgradeResponse{Version: version, Changed: true, RestartScheduled: true})
}

func (b broker) restartAfterUpgrade(ctx context.Context) error {
	if err := sleepContext(ctx, upgradeRestartWait); err != nil {
		return err
	}
	return serviceAction(ctx, "vps-agent", "restart")
}

func (b broker) scheduleUpgradeRestart() error {
	command := exec.Command(b.selfPath, "upgrade-restart")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func resolveLatestRelease(ctx context.Context) (string, error) {
	data, err := downloadURL(ctx, releaseAPIURL, maxChecksumBytes)
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	var release latestRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	if !validReleaseTag(release.TagName) {
		return "", fmt.Errorf("latest release returned invalid tag %q", release.TagName)
	}
	return release.TagName, nil
}

func validReleaseTag(tag string) bool {
	_, err := parseReleaseVersion(tag)
	return err == nil
}

func parseReleaseVersion(tag string) ([3]uint64, error) {
	var parsed [3]uint64
	if len(tag) < 2 || tag[0] != 'v' {
		return parsed, errors.New("release tag must start with v")
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != 3 {
		return parsed, errors.New("release tag must contain three numeric components")
	}
	for index, part := range parts {
		if part == "" {
			return parsed, errors.New("release version component is empty")
		}
		value, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return parsed, errors.New("release version component is not numeric")
		}
		parsed[index] = value
	}
	return parsed, nil
}

func compareReleaseVersions(left, right [3]uint64) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func downloadVerifiedAsset(ctx context.Context, tag, asset string, checksums map[string]string) ([]byte, error) {
	expected, ok := checksums[asset]
	if !ok {
		return nil, fmt.Errorf("release checksum list is missing %s", asset)
	}
	data, err := downloadReleaseAsset(ctx, tag, asset, maxBinaryBytes)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expected {
		return nil, fmt.Errorf("checksum verification failed for %s", asset)
	}
	return data, nil
}

func downloadReleaseAsset(ctx context.Context, tag, asset string, limit int64) ([]byte, error) {
	url := releaseDownloadURL + "/" + tag + "/" + asset
	data, err := downloadURL(ctx, url, limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	return data, nil
}

func downloadURL(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "vps-tool-agent-updater")
	client := &http.Client{
		Timeout: upgradeHTTPTimeout,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("too many HTTP redirects")
			}
			if request.URL.Scheme != "https" || !allowedReleaseHost(request.URL.Hostname()) {
				return errors.New("release download redirected to an untrusted host")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, errors.New("download exceeds the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download exceeds the size limit")
	}
	return data, nil
}

func allowedReleaseHost(host string) bool {
	host = strings.ToLower(host)
	return host == "api.github.com" || host == "github.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

func parseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			continue
		}
		checksums[name] = strings.ToLower(fields[0])
	}
	if len(checksums) == 0 {
		return nil, errors.New("release checksum list is invalid")
	}
	return checksums, nil
}

func prepareBinaryUpdate(target string, data []byte) (binaryUpdate, error) {
	update := binaryUpdate{target: target, backup: target + ".previous"}
	info, err := os.Lstat(target)
	if err != nil {
		return update, fmt.Errorf("inspect installed binary %s: %w", target, err)
	}
	if !info.Mode().IsRegular() || !trustedOwner(info) || info.Mode().Perm()&022 != 0 {
		return update, fmt.Errorf("installed binary %s failed ownership or permission checks", target)
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return update, fmt.Errorf("read installed binary %s: %w", target, err)
	}
	currentHash := sha256.Sum256(current)
	newHash := sha256.Sum256(data)
	if currentHash == newHash {
		return update, nil
	}

	file, err := os.CreateTemp(filepath.Dir(target), ".vps-agent-upgrade-*")
	if err != nil {
		return update, fmt.Errorf("stage binary %s: %w", target, err)
	}
	update.temp = file.Name()
	if err := file.Chmod(0o755); err != nil {
		file.Close()
		return update, fmt.Errorf("set staged binary permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return update, fmt.Errorf("write staged binary: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return update, fmt.Errorf("sync staged binary: %w", err)
	}
	if err := file.Close(); err != nil {
		return update, fmt.Errorf("close staged binary: %w", err)
	}
	update.changed = true
	return update, nil
}

func commitBinaryUpdates(updates []binaryUpdate) error {
	for _, update := range updates {
		if !update.changed {
			continue
		}
		if err := os.Remove(update.backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove previous backup: %w", err)
		}
		if err := os.Link(update.target, update.backup); err != nil {
			return fmt.Errorf("backup %s: %w", update.target, err)
		}
	}
	for _, update := range updates {
		if !update.changed {
			continue
		}
		if err := os.Rename(update.temp, update.target); err != nil {
			rollbackBinaryUpdates(updates)
			return fmt.Errorf("activate %s: %w", update.target, err)
		}
	}
	return nil
}

func rollbackBinaryUpdates(updates []binaryUpdate) {
	for _, update := range updates {
		if !update.changed {
			continue
		}
		if _, err := os.Stat(update.backup); err == nil {
			_ = os.Rename(update.backup, update.target)
		}
	}
}

func cleanupStagedUpdates(updates []binaryUpdate) {
	for _, update := range updates {
		if update.temp != "" {
			_ = os.Remove(update.temp)
		}
	}
}
