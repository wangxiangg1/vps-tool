package nodes

import (
	"context"
	"path/filepath"
	"testing"

	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
)

func TestEnrollmentIsOneTime(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: d, Cfg: config.Settings{EnrollmentTTL: 600}}
	n, token, _, err := s.Create("node", "", []string{}, "generic", "x-ui", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := n["id"].(string)
	_, cred, err := s.Enroll(id, token)
	if err != nil || cred == "" {
		t.Fatalf("enroll: %v", err)
	}
	if _, ok := s.AuthCredential(id, cred); !ok {
		t.Fatal("credential not accepted")
	}
	if _, _, err = s.Enroll(id, token); err == nil {
		t.Fatal("token was accepted twice")
	}
}

func TestUpdateStatusWarpExitCalibration(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: d, Cfg: config.Settings{EnrollmentTTL: 600}}
	n, _, _, err := s.Create("node", "", []string{}, "generic", "x-ui", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := n["id"].(string)

	// 模拟客户端上报 warp_status 为 off，但出口 IP 为 Cloudflare WARP 段
	report := map[string]any{
		"warp_status": "off",
		"public_ipv4": "104.28.195.192",
		"public_ipv6": "2a09:bac5:636f:3141::4e8:5e",
	}
	if err := s.UpdateStatus(id, report); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	fetched, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get node failed: %v", err)
	}
	if fetched["warp_status"] != "on" {
		t.Fatalf("expected warp_status to be 'on', got %v", fetched["warp_status"])
	}
}
