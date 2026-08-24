package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"vps-tool/agent/internal/validation"
)

const (
	DefaultStatePath = "/var/lib/vps-agent/requests.json"
	DefaultVersion   = "0.3.2"
	maxConfigBytes   = 64 * 1024
)

var supportedAdapters = map[string]struct{}{
	"generic":      {},
	"fixed-helper": {},
	"wgcf":         {},
	"warp-cli":     {},
	"warp-go":      {},
	"dry-run":      {},
}

// Config contains only local agent configuration. Credential is intentionally
// kept separate from the WSS URL and is sent in an Authorization header.
type Config struct {
	NodeID            string
	Credential        string
	RegistrationToken string
	WSSURL            string
	WarpAdapter       string
	XUIUnit           string
	HelperPath        string
	StatePath         string
	AgentVersion      string
	DryRun            bool
}

type fileConfig struct {
	NodeID            string `json:"node_id"`
	Credential        string `json:"credential"`
	RegistrationToken string `json:"registration_token"`
	WSSURL            string `json:"wss_url"`
	WarpAdapter       string `json:"warp_adapter"`
	XUIUnit           string `json:"xui_unit"`
	HelperPath        string `json:"helper_path"`
	StatePath         string `json:"state_path"`
	AgentVersion      string `json:"agent_version"`
	DryRun            *bool  `json:"dry_run"`
}

// Load reads a JSON file when it exists and then applies explicit environment
// overrides. A missing file is allowed so an installation can use only a
// restricted environment, but all required values are still validated.
func Load(path string) (Config, error) {
	values := fileConfig{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if len(data) > maxConfigBytes {
				return Config{}, fmt.Errorf("config file exceeds %d bytes", maxConfigBytes)
			}
			if err := checkRestrictedFile(path); err != nil {
				return Config{}, err
			}
			if err := decodeStrict(data, &values); err != nil {
				return Config{}, fmt.Errorf("decode config: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
	}

	applyEnv(&values)
	if values.StatePath == "" {
		values.StatePath = DefaultStatePath
	}
	if values.AgentVersion == "" {
		values.AgentVersion = DefaultVersion
	}
	if values.DryRun == nil {
		values.DryRun = boolPtr(false)
	}

	cfg := Config{
		NodeID:            values.NodeID,
		Credential:        values.Credential,
		RegistrationToken: values.RegistrationToken,
		WSSURL:            values.WSSURL,
		WarpAdapter:       values.WarpAdapter,
		XUIUnit:           values.XUIUnit,
		HelperPath:        values.HelperPath,
		StatePath:         values.StatePath,
		AgentVersion:      values.AgentVersion,
		DryRun:            *values.DryRun,
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Validate(cfg Config) error {
	if err := validation.NodeID(cfg.NodeID); err != nil {
		return err
	}
	if (cfg.Credential == "") == (cfg.RegistrationToken == "") {
		return fmt.Errorf("exactly one of credential or registration_token is required")
	}
	if err := validateSecret(cfg.Credential, "credential"); err != nil {
		return err
	}
	if err := validateSecret(cfg.RegistrationToken, "registration_token"); err != nil {
		return err
	}
	if err := validateWSSURL(cfg.WSSURL); err != nil {
		return err
	}
	if _, ok := supportedAdapters[cfg.WarpAdapter]; !ok {
		return fmt.Errorf("unsupported warp_adapter %q", cfg.WarpAdapter)
	}
	if cfg.WarpAdapter == "dry-run" && !cfg.DryRun {
		return fmt.Errorf("dry-run adapter requires dry_run=true")
	}
	if err := validation.UnitName(cfg.XUIUnit); err != nil {
		return err
	}
	if !cfg.DryRun {
		if err := validation.AbsolutePath(cfg.HelperPath, "helper_path"); err != nil {
			return err
		}
	} else if cfg.HelperPath != "" {
		if err := validation.AbsolutePath(cfg.HelperPath, "helper_path"); err != nil {
			return err
		}
	}
	if err := validation.AbsolutePath(cfg.StatePath, "state_path"); err != nil {
		return err
	}
	if cfg.AgentVersion == "" || len(cfg.AgentVersion) > 64 {
		return fmt.Errorf("invalid agent_version")
	}
	return nil
}

func validateWSSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("wss_url must be a wss:// URL without credentials, query, or fragment")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return fmt.Errorf("wss_url contains invalid control characters")
	}
	return nil
}

func applyEnv(values *fileConfig) {
	setString(&values.NodeID, "VPS_AGENT_NODE_ID")
	setString(&values.Credential, "VPS_AGENT_CREDENTIAL")
	setString(&values.RegistrationToken, "VPS_AGENT_REGISTRATION_TOKEN")
	setString(&values.WSSURL, "VPS_AGENT_WSS_URL")
	setString(&values.WarpAdapter, "VPS_AGENT_WARP_ADAPTER")
	setString(&values.XUIUnit, "VPS_AGENT_XUI_UNIT")
	setString(&values.HelperPath, "VPS_AGENT_HELPER_PATH")
	setString(&values.StatePath, "VPS_AGENT_STATE_PATH")
	setString(&values.AgentVersion, "VPS_AGENT_VERSION")
	if raw := os.Getenv("VPS_AGENT_DRY_RUN"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err == nil {
			values.DryRun = &parsed
		}
	}
}

func setString(target *string, name string) {
	if value := os.Getenv(name); value != "" {
		*target = value
	}
}

func validateSecret(value, name string) error {
	if value == "" {
		return nil
	}
	if len(value) > 4096 || strings.IndexFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return fmt.Errorf("%s must not contain control characters and must be at most 4096 bytes", name)
	}
	return nil
}

// PersistCredential atomically replaces a file-based registration token with
// the credential issued by the control plane. Environment-only registrations
// are rejected because they cannot survive an Agent restart safely.
func PersistCredential(path, credential string) error {
	if path == "" {
		return errors.New("registration_token requires a file config path")
	}
	if err := validateSecret(credential, "credential"); err != nil || credential == "" {
		if err == nil {
			err = errors.New("issued credential is empty")
		}
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config for credential persistence: %w", err)
	}
	values := fileConfig{}
	if err := decodeStrict(data, &values); err != nil {
		return fmt.Errorf("decode config for credential persistence: %w", err)
	}
	values.Credential = credential
	values.RegistrationToken = ""
	encoded, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config for credential persistence: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".vps-agent-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary config: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace config with issued credential: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0600)
	}
	return nil
}

func boolPtr(value bool) *bool { return &value }

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("config contains trailing JSON")
		}
		return err
	}
	return nil
}

func checkRestrictedFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("config file must not be group/world accessible")
	}
	return nil
}
