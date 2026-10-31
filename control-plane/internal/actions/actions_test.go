package actions

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/nodes"
)

func TestResultHistoryAndConcurrentRequestListing(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{EnrollmentTTL: 600, ActionTTL: 180}
	ns := &nodes.Service{DB: database, Cfg: cfg}
	s := &Service{DB: database, Cfg: cfg, Nodes: ns}
	node, _, _, err := ns.Create("node", "", []string{}, "generic", "x-ui", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := node["id"].(string)
	request, err := s.Create(context.Background(), id, "change_ip", map[string]any{}, "manual", "", true)
	if err != nil {
		t.Fatal(err)
	}
	requestID := request["id"].(string)
	result := map[string]any{"old_ip": "192.0.2.1", "new_ip": "192.0.2.2", "attempts": 1, "final_warp_state": "on"}
	if err := s.Finish(id, requestID, true, result, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(id, requestID, true, result, "", ""); err != nil {
		t.Fatal(err)
	}
	history := ns.History(id)
	if len(history) != 1 || history[0]["request_id"] != requestID || history[0]["old_ip"] != "192.0.2.1" || history[0]["new_ip"] != "192.0.2.2" {
		t.Fatalf("history: %+v", history)
	}
	var count int
	if err := database.SQL.QueryRow("SELECT count(*) FROM action_results WHERE request_id=?", requestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("result count=%d: %v", count, err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if len(s.List(id)) == 0 {
					t.Error("request list unexpectedly empty")
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent request listing did not finish")
	}
}
