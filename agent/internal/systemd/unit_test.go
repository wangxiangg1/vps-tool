package systemd

import "testing"

func TestNormalizeUnit(t *testing.T) {
	got, err := NormalizeUnit("x-ui")
	if err != nil || got != "x-ui.service" {
		t.Fatalf("NormalizeUnit(x-ui) = %q, %v", got, err)
	}
	for _, value := range []string{
		"x-ui.service; restart other.service",
		"../../x-ui.service",
		"--no-pager",
		"x-ui.service extra",
		"",
	} {
		if err := ValidateUnit(value); err == nil {
			t.Errorf("ValidateUnit(%q) accepted an unsafe unit", value)
		}
	}
}
