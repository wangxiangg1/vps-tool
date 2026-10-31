package scheduler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/nodes"
)

func TestNextDailyAndWeekly(t *testing.T) {
	ref := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	daily, err := next("daily", "10:30", "UTC", ref)
	if err != nil || !strings.HasPrefix(daily, "2026-10-31T10:30") {
		t.Fatalf("daily next=%q err=%v", daily, err)
	}
	weekly, err := next("weekly", "0 09:00", "UTC", ref)
	if err != nil || !strings.HasPrefix(weekly, "2026-11-02T09:00") {
		t.Fatalf("weekly next=%q err=%v", weekly, err)
	}
}

func TestLeaseAndRetryDoNotRepeatSuccessfulNodes(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES(?,?,?,?)", id, id, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Settings{SchedulerInterval: 5, ActionTTL: 180}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	ts := &TaskService{DB: d, Nodes: ns, Actions: as}
	s := &Scheduler{DB: d, Tasks: ts, Actions: as, Cfg: cfg}
	other := &Scheduler{DB: d, Tasks: ts, Actions: as, Cfg: cfg}
	if !s.lease() || other.lease() {
		t.Fatal("lease ownership was not enforced")
	}
	m, err := ts.Create("test", []string{"one", "two"}, "warp_on", map[string]any{}, "daily", "10:00", "UTC", true, 1, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	id := m["id"].(string)
	if _, err = d.SQL.Exec("UPDATE tasks SET next_run_at=? WHERE id=?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	s.run(context.Background(), id)
	var runID string
	if err = d.SQL.QueryRow("SELECT id FROM task_runs WHERE task_id=?", id).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SQL.Exec("UPDATE action_requests SET status='succeeded' WHERE task_run_id=? AND node_id='one'", runID); err != nil {
		t.Fatal(err)
	}
	s.processRuns(context.Background())
	var status, summary string
	if err = d.SQL.QueryRow("SELECT status,summary_json FROM task_runs WHERE id=?", runID).Scan(&status, &summary); err != nil {
		t.Fatal(err)
	}
	if status != "retry_wait" || !strings.Contains(summary, `"two"`) {
		t.Fatalf("status=%s summary=%s", status, summary)
	}
	time.Sleep(1100 * time.Millisecond)
	s.processRuns(context.Background())
	s.processRuns(context.Background())
	var one, two int
	if err = d.SQL.QueryRow("SELECT count(*) FROM action_requests WHERE task_run_id=? AND node_id='one'", runID).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if err = d.SQL.QueryRow("SELECT count(*) FROM action_requests WHERE task_run_id=? AND node_id='two'", runID).Scan(&two); err != nil {
		t.Fatal(err)
	}
	if one != 1 || two != 2 {
		t.Fatalf("unexpected requests: one=%d two=%d", one, two)
	}
	if err = d.SQL.QueryRow("SELECT status FROM task_runs WHERE id=?", runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("status=%s", status)
	}
}

func TestCronValidation(t *testing.T) {
	if err := validate("cron", "*/5 * * * *", "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	if err := validate("cron", "bad expression", "UTC"); err == nil {
		t.Fatal("invalid cron accepted")
	}
	if err := validate("cron", "@daily", "UTC"); err == nil {
		t.Fatal("cron descriptor accepted")
	}
	ref := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	n, err := next("cron", "0 9 * * 0", "UTC", ref)
	if err != nil || !strings.HasPrefix(n, "2026-11-02T09:00") {
		t.Fatalf("Monday cron next=%q err=%v", n, err)
	}
}

func TestTaskCRUDAndLeaseTakeover(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES('n','node','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{SchedulerInterval: 5, ActionTTL: 180}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	ts := &TaskService{DB: d, Nodes: ns, Actions: as}
	task, err := ts.Create("task", []string{"n"}, "change_ip", map[string]any{}, "daily", "10:00", "UTC", true, 1, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	id := task["id"].(string)
	updated, err := ts.Update(id, map[string]any{"enabled": false})
	if err != nil || updated["next_run_at"] != nil {
		t.Fatalf("disable: %v %v", updated, err)
	}
	updated, err = ts.Update(id, map[string]any{"enabled": true, "schedule_type": "cron", "schedule_value": "0 9 * * 0"})
	if err != nil || updated["next_run_at"] == nil {
		t.Fatalf("enable: %v %v", updated, err)
	}
	if len(ts.List()) != 1 || !ts.Delete(id) || len(ts.List()) != 0 {
		t.Fatal("task CRUD mismatch")
	}
	a := &Scheduler{DB: d, Cfg: cfg}
	b := &Scheduler{DB: d, Cfg: cfg}
	if !a.lease() || b.lease() {
		t.Fatal("lease must exclude another owner")
	}
	if _, err = d.SQL.Exec("UPDATE scheduler_leases SET lease_until='2020-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	if !b.lease() {
		t.Fatal("expired lease should be acquired")
	}
}

func TestMissedTaskDoesNotDispatch(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES('n','node','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{SchedulerInterval: 5, ActionTTL: 180}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	ts := &TaskService{DB: d, Nodes: ns, Actions: as}
	s := &Scheduler{DB: d, Tasks: ts, Actions: as, Cfg: cfg}
	task, err := ts.Create("task", []string{"n"}, "warp_on", map[string]any{}, "daily", "10:00", "UTC", true, 2, []int{30, 90})
	if err != nil {
		t.Fatal(err)
	}
	id := task["id"].(string)
	_, err = d.SQL.Exec("UPDATE tasks SET next_run_at=? WHERE id=?", time.Now().UTC().Add(-6*time.Minute).Format(time.RFC3339Nano), id)
	if err != nil {
		t.Fatal(err)
	}
	s.run(context.Background(), id)
	var status string
	var count int
	if err = d.SQL.QueryRow("SELECT status FROM task_runs WHERE task_id=?", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = d.SQL.QueryRow("SELECT COUNT(*) FROM action_requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if status != "missed" || count != 0 {
		t.Fatalf("status=%s requests=%d", status, count)
	}
}

func TestUnknownResultIsNotRetried(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES('n','node','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Settings{SchedulerInterval: 5, ActionTTL: 180}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	ts := &TaskService{DB: d, Nodes: ns, Actions: as}
	s := &Scheduler{DB: d, Tasks: ts, Actions: as, Cfg: cfg}
	task, err := ts.Create("task", []string{"n"}, "warp_on", map[string]any{}, "daily", "10:00", "UTC", true, 2, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	id := task["id"].(string)
	_, err = d.SQL.Exec("UPDATE tasks SET next_run_at=? WHERE id=?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), id)
	if err != nil {
		t.Fatal(err)
	}
	s.run(context.Background(), id)
	_, err = d.SQL.Exec("UPDATE action_requests SET status='unknown'")
	if err != nil {
		t.Fatal(err)
	}
	s.processRuns(context.Background())
	var status string
	var count int
	if err = d.SQL.QueryRow("SELECT status FROM task_runs WHERE task_id=?", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = d.SQL.QueryRow("SELECT COUNT(*) FROM action_requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if status != "dispatched" || count != 1 {
		t.Fatalf("status=%s requests=%d", status, count)
	}
}

func TestRetryPreservesEarlierCreationErrors(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"busy", "retry"} {
		if _, err = d.SQL.Exec("INSERT INTO nodes(id,name,created_at,updated_at) VALUES(?,?,?,?)", id, id, "2026-01-01", "2026-01-01"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Settings{SchedulerInterval: 5, ActionTTL: 180}
	ns := &nodes.Service{DB: d, Cfg: cfg}
	as := &actions.Service{DB: d, Cfg: cfg, Nodes: ns}
	ts := &TaskService{DB: d, Nodes: ns, Actions: as}
	s := &Scheduler{DB: d, Tasks: ts, Actions: as, Cfg: cfg}
	if _, err = as.Create(context.Background(), "busy", "warp_on", map[string]any{}, "manual", "", true); err != nil {
		t.Fatal(err)
	}
	task, err := ts.Create("task", []string{"busy", "retry"}, "warp_on", map[string]any{}, "daily", "10:00", "UTC", true, 1, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.SQL.Exec("UPDATE tasks SET next_run_at=? WHERE id=?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), task["id"]); err != nil {
		t.Fatal(err)
	}
	s.run(context.Background(), task["id"].(string))
	s.processRuns(context.Background())
	if _, err = d.SQL.Exec(`UPDATE task_runs SET summary_json=json_set(summary_json,'$.retry_at','2000-01-01T00:00:00Z') WHERE task_id=?`, task["id"]); err != nil {
		t.Fatal(err)
	}
	s.processRuns(context.Background())
	if _, err = d.SQL.Exec("UPDATE action_requests SET status='succeeded' WHERE task_run_id IS NOT NULL AND node_id='retry'"); err != nil {
		t.Fatal(err)
	}
	s.processRuns(context.Background())
	var status string
	if err = d.SQL.QueryRow("SELECT status FROM task_runs WHERE task_id=?", task["id"]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("status=%s, want failed due to earlier action_busy", status)
	}
}
