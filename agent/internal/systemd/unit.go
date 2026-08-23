package systemd

import (
	"fmt"
	"regexp"
	"strings"
)

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,120}$`)

// NormalizeUnit accepts a single service unit name and never accepts a path
// or an option. A missing .service suffix is added for operator convenience.
func NormalizeUnit(value string) (string, error) {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\\x00\r\n\t ") {
		return "", fmt.Errorf("invalid systemd unit")
	}
	if !unitPattern.MatchString(value) {
		return "", fmt.Errorf("invalid systemd unit")
	}
	if strings.HasSuffix(value, ".service") {
		return value, nil
	}
	return value + ".service", nil
}

func ValidateUnit(value string) error {
	_, err := NormalizeUnit(value)
	return err
}
