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
