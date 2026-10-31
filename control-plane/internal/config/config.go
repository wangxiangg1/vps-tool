package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Settings struct {
	DBPath            string
	BackupDir         string
	AdminUser         string
	AdminPassword     string
	CookieSecure      bool
	CookieName        string
	SessionTTL        int
	EnrollmentTTL     int
	ActionTTL         int
	HeartbeatTimeout  int
	SchedulerInterval int
	ListenAddr        string
}

func Load() (Settings, error) {
	s := Settings{DBPath: env("VPS_TOOL_DB_PATH", "./data/vps-tool.sqlite3"), CookieName: env("VPS_TOOL_SESSION_COOKIE", "vps_tool_session"), CookieSecure: true, SessionTTL: 28800, EnrollmentTTL: 600, ActionTTL: 180, HeartbeatTimeout: 90, SchedulerInterval: 5, ListenAddr: env("VPS_TOOL_LISTEN_ADDR", ":8000")}
	s.AdminUser, s.AdminPassword = strings.TrimSpace(os.Getenv("VPS_TOOL_ADMIN_USER")), os.Getenv("VPS_TOOL_ADMIN_PASSWORD")
	if s.AdminUser == "" || s.AdminPassword == "" {
		return s, fmt.Errorf("VPS_TOOL_ADMIN_USER and VPS_TOOL_ADMIN_PASSWORD must be configured")
	}
	if n := len([]byte(s.AdminPassword)); n < 12 || n > 72 {
		return s, fmt.Errorf("VPS_TOOL_ADMIN_PASSWORD must contain 12 to 72 UTF-8 bytes")
	}
	if v, ok, err := intEnv("VPS_TOOL_SESSION_TTL_SECONDS", s.SessionTTL); err != nil {
		return s, err
	} else if ok {
		s.SessionTTL = max(v, 300)
	}
	if v, ok, err := intEnv("VPS_TOOL_ENROLLMENT_TTL", s.EnrollmentTTL); err != nil {
		return s, err
	} else if ok {
		s.EnrollmentTTL = max(v, 60)
	}
	if v, ok, err := intEnv("VPS_TOOL_ACTION_TTL", s.ActionTTL); err != nil {
		return s, err
	} else if ok {
		s.ActionTTL = max(v, 30)
	}
	if v, ok, err := intEnv("VPS_TOOL_HEARTBEAT_TIMEOUT", s.HeartbeatTimeout); err != nil {
		return s, err
	} else if ok {
		s.HeartbeatTimeout = max(v, 30)
	}
	if v, ok, err := intEnv("VPS_TOOL_SCHEDULER_INTERVAL", s.SchedulerInterval); err != nil {
		return s, err
	} else if ok {
		s.SchedulerInterval = max(v, 1)
	}
	if v := os.Getenv("VPS_TOOL_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("VPS_TOOL_COOKIE_SECURE must be boolean")
		}
		s.CookieSecure = b
	}
	if !filepath.IsAbs(s.DBPath) {
		s.DBPath, _ = filepath.Abs(s.DBPath)
	}
	s.BackupDir = env("VPS_TOOL_BACKUP_DIR", filepath.Join(filepath.Dir(s.DBPath), "backups"))
	if !filepath.IsAbs(s.BackupDir) {
		s.BackupDir, _ = filepath.Abs(s.BackupDir)
	}
	return s, nil
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func intEnv(k string, d int) (int, bool, error) {
	v := os.Getenv(k)
	if v == "" {
		return d, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, true, fmt.Errorf("%s must be an integer", k)
	}
	return n, true, nil
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
