package validation

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	nodeIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	unitPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,127}$`)
	adapterPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

func NodeID(value string) error {
	if !nodeIDPattern.MatchString(value) {
		return fmt.Errorf("node_id must contain only letters, digits, '.', '_', ':', or '-' and be 1-128 characters")
	}
	return nil
}

// UnitName accepts a single systemd unit token. It deliberately excludes
// paths, whitespace and option-like values so it can never become a command
// line fragment.
func UnitName(value string) error {
	if !unitPattern.MatchString(value) || strings.ContainsAny(value, `/\\`) || strings.Contains(value, "..") {
		return fmt.Errorf("invalid systemd unit name")
	}
	return nil
}

func AdapterName(value string) error {
	if !adapterPattern.MatchString(value) {
		return fmt.Errorf("invalid warp adapter name")
	}
	return nil
}

func AbsolutePath(value, field string) error {
	if value == "" || !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must be an absolute local path", field)
	}
	clean := filepath.Clean(value)
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return fmt.Errorf("%s must not contain parent-directory traversal", field)
		}
	}
	return nil
}

func RequestID(value string) error {
	if !nodeIDPattern.MatchString(value) {
		return fmt.Errorf("invalid request_id")
	}
	return nil
}
