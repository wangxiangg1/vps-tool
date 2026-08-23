package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRejectsCredentialInWSSURL(t *testing.T) {
	cfg := Config{
		NodeID:       "node-1",
		Credential:   "secret",
		WSSURL:       "wss://user:secret@example.com/agent",
		WarpAdapter:  "fixed-helper",
		XUIUnit:      "x-ui.service",
		HelperPath:   "/usr/local/libexec/vps-agent-helper",
		StatePath:    "/var/lib/vps-agent/requests.json",
		AgentVersion: DefaultVersion,
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("credential-bearing WSS URL was accepted")
	}
}

func TestRegistrationTokenIsPersistedAsCredential(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "agent.json")
	statePath := filepath.Join(tempDir, "state.json")
	data := []byte(fmt.Sprintf(`{"node_id":"node-1","registration_token":"registration-token","wss_url":"wss://example.com/agent","warp_adapter":"dry-run","xui_unit":"x-ui.service","state_path":%q,"agent_version":"%s","dry_run":true}`, statePath, DefaultVersion))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationToken != "registration-token" || cfg.Credential != "" {
		t.Fatalf("registration config = %#v", cfg)
	}
	if err := PersistCredential(path, "issued-credential"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Credential != "issued-credential" || reloaded.RegistrationToken != "" {
		t.Fatalf("persisted config = %#v", reloaded)
	}
}

func TestLoadRejectsUnknownConfigFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"node_id":"node-1","credential":"secret","wss_url":"wss://example.com/agent","warp_adapter":"fixed-helper","xui_unit":"x-ui.service","helper_path":"/helper","state_path":"/state","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field was accepted")
	}
}
