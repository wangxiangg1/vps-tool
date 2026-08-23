package backoff

import (
	"testing"
	"time"
)

func TestNextUsesBoundedExponentialScheduleWithJitter(t *testing.T) {
	policy := New(7)
	previous := time.Duration(0)
	for index, base := range Schedule() {
		next := policy.Next()
		lower := time.Duration(float64(base) * 0.8)
		upper := time.Duration(float64(base) * 1.2)
		if next < lower || next > upper {
			t.Fatalf("attempt %d delay %s outside [%s, %s]", index, next, lower, upper)
		}
		previous = next
	}
	for index := 0; index < 3; index++ {
		next := policy.Next()
		base := Schedule()[len(Schedule())-1]
		if next < time.Duration(float64(base)*0.8) || next > time.Duration(float64(base)*1.2) {
			t.Fatalf("capped attempt %d delay %s outside cap jitter", index, next)
		}
		if previous == 0 {
			t.Fatal("invalid previous delay")
		}
	}
	policy.Reset()
	if got := policy.Next(); got < time.Duration(float64(Schedule()[0])*.8) {
		t.Fatalf("reset did not restart schedule: %s", got)
	}
}
