package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalPersistsDeduplicationAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.json")
	j, err := Open(path, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 3; index++ {
		if err := j.Record(Entry{
			RequestID:  "request-" + string(rune('0'+index)),
			Action:     "get_status",
			State:      "succeeded",
			Code:       "ok",
			Message:    "done",
			Result:     json.RawMessage(`{"value":"bounded"}`),
			FinishedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if j.Count() != 2 {
		t.Fatalf("journal count = %d, want 2", j.Count())
	}
	if _, ok := j.Lookup("request-1"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if _, ok := j.Lookup("request-3"); !ok {
		t.Fatal("newest entry is missing")
	}
	if err := j.Record(Entry{
		RequestID: "request-3",
		Action:    "get_status",
		State:     "failed",
		Code:      "changed",
		Message:   "must not overwrite",
		Result:    json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("same request_id should be idempotent: %v", err)
	}
	if err := j.Record(Entry{
		RequestID: "request-3",
		Action:    "warp_on",
		State:     "succeeded",
		Code:      "ok",
		Message:   "conflict",
		Result:    json.RawMessage(`{}`),
	}); err != ErrRequestConflict {
		t.Fatalf("different action conflict = %v, want ErrRequestConflict", err)
	}

	reopened, err := Open(path, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Lookup("request-3")
	if !ok || entry.Code != "ok" {
		t.Fatalf("reopened journal entry = %#v, %v", entry, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("journal size = %d, exceeds limit", info.Size())
	}
}

func TestJournalRejectsOversizedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.json")
	j, err := Open(path, 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Record(Entry{
		RequestID: "large",
		Action:    "get_status",
		State:     "succeeded",
		Code:      "ok",
		Message:   "done",
		Result:    json.RawMessage(`{"large":"` + strings.Repeat("x", 2000) + `"}`),
	}); err == nil {
		t.Fatal("oversized entry was accepted")
	}
	if j.Count() != 0 {
		t.Fatal("oversized entry changed the in-memory journal")
	}
}
