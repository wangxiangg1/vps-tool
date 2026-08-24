package helper

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBackupPathPreparesPrivateDestination(t *testing.T) {
	runner := &Runner{StatePath: filepath.Join(t.TempDir(), "requests.json")}
	path, err := runner.BackupPath("56075d29-63c6-49d2-8747-dec7d6e6bc33")
	if err != nil {
		t.Fatalf("BackupPath() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("prepared backup stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("prepared backup mode = %o, want 600", got)
		}
	}
}
