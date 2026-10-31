package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/nodes"
)

var scheduledActions = map[string]bool{"warp_on": true, "warp_off": true, "change_ip": true, "restart_xui": true}

type ScheduleError struct{ Message string }

func (e *ScheduleError) Error() string { return e.Message }

type TaskService struct {
	DB      *db.DB
	Nodes   *nodes.Service
	Actions *actions.Service
}

func clock(v string) (int, int, error) {
	p := strings.Split(strings.TrimSpace(v), ":")
	if len(p) != 2 {
		return 0, 0, &ScheduleError{"schedule time must use HH:MM"}
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, &ScheduleError{"schedule time must be within 00:00 and 23:59"}
	}
	return h, m, nil
}
func validate(kind, value, zone string) error {
	if _, e := time.LoadLocation(zone); e != nil {
		return &ScheduleError{"unknown IANA timezone: " + zone}
	}
	switch kind {
	case "daily":
		_, _, e := clock(value)
		return e
	case "weekly":
		p := strings.Fields(value)
		if len(p) != 2 {
			return &ScheduleError{"weekly schedule must use '<weekday 0-6> HH:MM'"}
		}
		d, e := strconv.Atoi(p[0])
		if e != nil || d < 0 || d > 6 {
			return &ScheduleError{"weekly weekday must be between 0 and 6"}
		}
		_, _, e = clock(p[1])
		return e
	case "cron":
		_, e := parseCron(value)
		if e != nil {
			return &ScheduleError{"invalid cron expression: " + e.Error()}
		}
		return nil
	}
	return &ScheduleError{"schedule_type must be daily, weekly, or cron"}
}

// The Python fallback treats weekday 0 as Monday. Convert its five-field
// weekday values to the Sunday-zero convention used by robfig/cron.
func parseCron(expression string) (cron.Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron requires five fields")
	}
	if fields[4] != "*" {
		values := map[int]bool{}
		for _, part := range strings.Split(fields[4], ",") {
			pieces := strings.Split(part, "/")
			if len(pieces) > 2 || len(pieces) == 0 {
				return nil, fmt.Errorf("invalid weekday")
			}
			step := 1
			if len(pieces) == 2 {
				var err error
				step, err = strconv.Atoi(pieces[1])
				if err != nil || step < 1 {
					return nil, fmt.Errorf("invalid weekday step")
				}
			}
			start, end := 0, 6
			if pieces[0] != "*" {
				bounds := strings.Split(pieces[0], "-")
				if len(bounds) > 2 || len(bounds) == 0 {
					return nil, fmt.Errorf("invalid weekday")
				}
				var err error
				start, err = strconv.Atoi(bounds[0])
				if err != nil {
					return nil, err
				}
				end = start
				if len(bounds) == 2 {
					end, err = strconv.Atoi(bounds[1])
					if err != nil {
						return nil, err
					}
				}
			}
			if start < 0 || end > 6 || start > end {
				return nil, fmt.Errorf("weekday out of range")
			}
			for n := start; n <= end; n += step {
				values[(n+1)%7] = true
			}
		}
		days := make([]int, 0, len(values))
		for n := range values {
			days = append(days, n)
		}
		sort.Ints(days)
		parts := make([]string, 0, len(days))
		for _, n := range days {
			parts = append(parts, strconv.Itoa(n))
		}
		fields[4] = strings.Join(parts, ",")
	}
	return cron.ParseStandard(strings.Join(fields, " "))
}
func next(kind, value, zone string, after time.Time) (string, error) {
	if e := validate(kind, value, zone); e != nil {
		return "", e
	}
	loc, _ := time.LoadLocation(zone)
	local := after.In(loc)
	if kind == "cron" {
		s, _ := parseCron(value)
		return s.Next(local).UTC().Format(time.RFC3339Nano), nil
	}
	weekday := -1
	clockValue := value
	if kind == "weekly" {
		p := strings.Fields(value)
		weekday, _ = strconv.Atoi(p[0])
		clockValue = p[1]
	}
	h, m, _ := clock(clockValue)
	for i := 0; i < 8; i++ {
		d := local.AddDate(0, 0, i)
		if weekday >= 0 && (int(d.Weekday())+6)%7 != weekday {
			continue
		}
		candidate := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if candidate.After(local) {
			return candidate.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", &ScheduleError{"could not calculate next schedule time"}
}

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (map[string]any, error) {
	var id, name, nodesJSON, action, params, kind, value, zone, retries string
	var enabled, maxRetries int
	var nextRun, created, updated, deleted sql.NullString
	e := r.Scan(&id, &name, &nodesJSON, &action, &params, &kind, &value, &zone, &enabled, &maxRetries, &retries, &nextRun, &created, &updated, &deleted)
	if e != nil {
		return nil, e
	}
	var ns, pa, ri any
	json.Unmarshal([]byte(nodesJSON), &ns)
	json.Unmarshal([]byte(params), &pa)
	json.Unmarshal([]byte(retries), &ri)
	var nextValue any
	if nextRun.Valid {
		nextValue = nextRun.String
	}
	return map[string]any{"id": id, "name": name, "node_ids": ns, "action": action, "parameters": pa, "schedule_type": kind, "schedule_value": value, "timezone": zone, "enabled": enabled == 1, "max_retries": maxRetries, "retry_intervals_seconds": ri, "next_run_at": nextValue, "created_at": created.String, "updated_at": updated.String}, nil
}
func (t *TaskService) List() []map[string]any {
	rows, e := t.DB.SQL.Query("SELECT * FROM tasks WHERE deleted_at IS NULL ORDER BY created_at DESC")
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if x, e := scanTask(rows); e == nil {
			out = append(out, x)
		}
	}
	return out
}
func (t *TaskService) Get(id string) (map[string]any, error) {
	return scanTask(t.DB.SQL.QueryRow("SELECT * FROM tasks WHERE id=? AND deleted_at IS NULL", id))
}
func (t *TaskService) Create(name string, nodeIDs []string, action string, params map[string]any, kind, value, zone string, enabled bool, maxRetries int, retry []int) (map[string]any, error) {
	if !scheduledActions[action] {
		return nil, &ScheduleError{"scheduled tasks only support state-changing actions"}
	}
	if len(name) == 0 || len([]rune(name)) > 100 || len(nodeIDs) == 0 || len(nodeIDs) > 100 || len(retry) > 3 {
		return nil, &ScheduleError{"invalid task fields"}
	}
	if params == nil {
		params = map[string]any{}
	}
	if err := t.Actions.Validate(action, params); err != nil {
		return nil, &ScheduleError{err.Error()}
	}
	seen := map[string]bool{}
	for _, id := range nodeIDs {
		if seen[id] {
			return nil, &ScheduleError{"node_ids must not contain duplicates"}
		}
		seen[id] = true
		if _, e := t.Nodes.Get(id); e != nil {
			return nil, &ScheduleError{"unknown node_id: " + id}
		}
	}
	if e := validate(kind, value, zone); e != nil {
		return nil, e
	}
	if maxRetries < 0 || maxRetries > 3 {
		return nil, &ScheduleError{"max_retries must be between 0 and 3"}
	}
	for _, v := range retry {
		if v < 1 || v > 86400 {
			return nil, &ScheduleError{"retry intervals must be between 1 and 86400 seconds"}
		}
	}
	nextRun := ""
	if enabled {
		nextRun, _ = next(kind, value, zone, time.Now().UTC())
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := uuid.NewString()
	var nr any = nextRun
	if nextRun == "" {
		nr = nil
	}
	_, e := t.DB.SQL.Exec(`INSERT INTO tasks(id,name,node_ids_json,action,parameters_json,schedule_type,schedule_value,timezone,enabled,max_retries,retry_intervals_json,next_run_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, name, db.JSON(nodeIDs), action, db.JSON(params), kind, value, zone, enabled, maxRetries, db.JSON(retry), nr, now, now)
	if e != nil {
		return nil, e
	}
	return t.Get(id)
}
func (t *TaskService) Update(id string, c map[string]any) (map[string]any, error) {
	old, e := t.Get(id)
	if e != nil {
		return nil, e
	}
	kind, _ := old["schedule_type"].(string)
	value, _ := old["schedule_value"].(string)
	zone, _ := old["timezone"].(string)
	if v, ok := c["schedule_type"].(string); ok {
		kind = v
	}
	if v, ok := c["schedule_value"].(string); ok {
		value = v
	}
	if v, ok := c["timezone"].(string); ok {
		zone = v
	}
	if e = validate(kind, value, zone); e != nil {
		return nil, e
	}
	allowed := map[string]string{"name": "name", "enabled": "enabled", "schedule_type": "schedule_type", "schedule_value": "schedule_value", "timezone": "timezone", "max_retries": "max_retries", "retry_intervals_seconds": "retry_intervals_json"}
	for k, v := range c {
		if _, ok := allowed[k]; !ok {
			return nil, &ScheduleError{"unsupported task fields"}
		}
		switch k {
		case "name", "schedule_type", "schedule_value", "timezone":
			text, ok := v.(string)
			if !ok || len(text) == 0 || len([]rune(text)) > 100 {
				return nil, &ScheduleError{"invalid task field: " + k}
			}
		case "enabled":
			if _, ok := v.(bool); !ok {
				return nil, &ScheduleError{"enabled must be boolean"}
			}
		case "max_retries":
			n, ok := v.(float64)
			if !ok || n != math.Trunc(n) || n < 0 || n > 3 {
				return nil, &ScheduleError{"max_retries must be between 0 and 3"}
			}
		case "retry_intervals_seconds":
			values, ok := v.([]any)
			if !ok || len(values) > 3 {
				return nil, &ScheduleError{"invalid retry intervals"}
			}
			for _, x := range values {
				n, ok := x.(float64)
				if !ok || n != math.Trunc(n) || n < 1 || n > 86400 {
					return nil, &ScheduleError{"retry intervals must be between 1 and 86400 seconds"}
				}
			}
		}
	}
	sets := []string{}
	args := []any{}
	for k, v := range c {
		col, ok := allowed[k]
		if !ok {
			continue
		}
		if k == "retry_intervals_seconds" {
			v = db.JSON(v)
		}
		if k == "enabled" {
			if b, ok := v.(bool); ok {
				if b {
					v = 1
				} else {
					v = 0
				}
			}
		}
		sets = append(sets, col+"=?")
		args = append(args, v)
	}
	if b, ok := c["enabled"].(bool); ok {
		if b {
			n, _ := next(kind, value, zone, time.Now().UTC())
			sets = append(sets, "next_run_at=?")
			args = append(args, n)
		} else {
			sets = append(sets, "next_run_at=NULL")
		}
	} else if old["enabled"] == true && (c["schedule_type"] != nil || c["schedule_value"] != nil || c["timezone"] != nil) {
		n, _ := next(kind, value, zone, time.Now().UTC())
		sets = append(sets, "next_run_at=?")
		args = append(args, n)
	}
	if len(sets) == 0 {
		return old, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	args = append(args, now, id)
	_, e = t.DB.SQL.Exec("UPDATE tasks SET "+strings.Join(sets, ",")+",updated_at=? WHERE id=? AND deleted_at IS NULL", args...)
	if e != nil {
		return nil, e
	}
	return t.Get(id)
}
func (t *TaskService) Delete(id string) bool {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r, e := t.DB.SQL.Exec("UPDATE tasks SET enabled=0,deleted_at=?,updated_at=? WHERE id=? AND deleted_at IS NULL", now, now, id)
	if e != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n > 0
}
func (t *TaskService) Runs(id string) []map[string]any {
	rows, e := t.DB.SQL.Query("SELECT id,task_id,scheduled_for,status,attempt,summary_json,created_at,started_at,finished_at FROM task_runs WHERE task_id=? ORDER BY created_at DESC LIMIT 100", id)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var i, ti, sf, st, summ, cr, sa, fi sql.NullString
		var attempt int
		if rows.Scan(&i, &ti, &sf, &st, &attempt, &summ, &cr, &sa, &fi) == nil {
			var summary any
			json.Unmarshal([]byte(summ.String), &summary)
			out = append(out, map[string]any{"id": i.String, "task_id": ti.String, "scheduled_for": sf.String, "status": st.String, "attempt": attempt, "summary": summary, "created_at": cr.String, "started_at": sa.String, "finished_at": fi.String})
		}
	}
	return out
}

type Scheduler struct {
	DB      *db.DB
	Tasks   *TaskService
	Actions *actions.Service
	Cfg     config.Settings
	owner   string
	mu      sync.Mutex
}

func (s *Scheduler) Run(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.Cfg.SchedulerInterval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.iterate(ctx)
		}
	}
}
func (s *Scheduler) lease() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == "" {
		s.owner = uuid.NewString()
	}
	conn, e := s.DB.SQL.Conn(context.Background())
	if e != nil {
		return false
	}
	defer conn.Close()
	if _, e = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); e != nil {
		return false
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	now := time.Now().UTC()
	var owner, until string
	e = conn.QueryRowContext(context.Background(), "SELECT owner_id,lease_until FROM scheduler_leases WHERE name='default'").Scan(&owner, &until)
	if e == nil {
		u, _ := time.Parse(time.RFC3339Nano, until)
		if owner != s.owner && u.After(now) {
			return false
		}
	}
	_, e = conn.ExecContext(context.Background(), `INSERT INTO scheduler_leases(name,owner_id,lease_until,updated_at) VALUES('default',?,?,?) ON CONFLICT(name) DO UPDATE SET owner_id=excluded.owner_id,lease_until=excluded.lease_until,updated_at=excluded.updated_at`, s.owner, now.Add(time.Duration(max(10, s.Cfg.SchedulerInterval*3))*time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if e != nil {
		return false
	}
	if _, e = conn.ExecContext(context.Background(), "COMMIT"); e != nil {
		return false
	}
	committed = true
	return true
}
func (s *Scheduler) iterate(ctx context.Context) {
	if !s.lease() {
		return
	}
	rows, e := s.DB.SQL.Query("SELECT id FROM tasks WHERE enabled=1 AND deleted_at IS NULL AND next_run_at IS NOT NULL AND julianday(next_run_at)<=julianday(?) ORDER BY next_run_at LIMIT 50", time.Now().UTC().Format(time.RFC3339Nano))
	if e != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.run(ctx, id)
	}
	s.processRuns(ctx)
	s.Actions.Expire()
}
func (s *Scheduler) run(ctx context.Context, id string) {
	task, e := s.Tasks.Get(id)
	if e != nil {
		return
	}
	at, ok := task["next_run_at"].(string)
	if !ok || at == "" {
		return
	}
	nextAt, _ := next(task["schedule_type"].(string), task["schedule_value"].(string), task["timezone"].(string), time.Now().UTC())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	runID := uuid.NewString()
	scheduledAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return
	}
	initialStatus := "running"
	if time.Since(scheduledAt) > 5*time.Minute {
		initialStatus = "missed"
	}
	conn, e := s.DB.SQL.Conn(ctx)
	if e != nil {
		return
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); e != nil {
		return
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var current string
	var enabled int
	if e = conn.QueryRowContext(ctx, "SELECT next_run_at,enabled FROM tasks WHERE id=? AND deleted_at IS NULL", id).Scan(&current, &enabled); e != nil || enabled != 1 || current != at {
		return
	}
	r, e := conn.ExecContext(ctx, `INSERT OR IGNORE INTO task_runs(id,task_id,scheduled_for,status,attempt,summary_json,created_at,started_at,finished_at) VALUES(?,?,?,?,0,'{}',?,?,?)`, runID, id, at, initialStatus, now, now, func() any {
		if initialStatus == "missed" {
			return now
		}
		return nil
	}())
	if e != nil {
		return
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return
	}
	if _, e = conn.ExecContext(ctx, "UPDATE tasks SET next_run_at=?,updated_at=? WHERE id=? AND next_run_at=?", nextAt, now, id, at); e != nil {
		return
	}
	if _, e = conn.ExecContext(ctx, "COMMIT"); e != nil {
		return
	}
	committed = true
	_ = conn.Close()
	if initialStatus == "missed" {
		return
	}
	nodesJSON, _ := json.Marshal(task["node_ids"])
	var nodeIDs []string
	json.Unmarshal(nodesJSON, &nodeIDs)
	params, _ := task["parameters"].(map[string]any)
	counts := map[string]int{}
	var creationErrors int
	for _, nodeID := range nodeIDs {
		requestID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%s:%d", runID, nodeID, 0))).String()
		m, e := s.Actions.Create(ctx, nodeID, task["action"].(string), params, "scheduled", requestID, false, id, runID)
		if e != nil {
			counts[e.Error()]++
			creationErrors++
		} else {
			counts[m["status"].(string)]++
		}
	}
	s.DB.SQL.Exec("UPDATE task_runs SET status='dispatched',summary_json=?,finished_at=NULL WHERE id=?", db.JSON(map[string]any{"counts": counts, "creation_errors": creationErrors}), runID)
}

func (s *Scheduler) processRuns(ctx context.Context) {
	type run struct {
		id, taskID, status, summary, scheduledAt string
		attempt                                  int
	}
	rows, err := s.DB.SQL.Query("SELECT id,task_id,status,attempt,summary_json,scheduled_for FROM task_runs WHERE status IN ('dispatched','running','retry_wait') ORDER BY created_at LIMIT 100")
	if err != nil {
		return
	}
	var runs []run
	for rows.Next() {
		var r run
		if rows.Scan(&r.id, &r.taskID, &r.status, &r.attempt, &r.summary, &r.scheduledAt) == nil {
			runs = append(runs, r)
		}
	}
	rows.Close()
	for _, r := range runs {
		task, err := s.Tasks.Get(r.taskID)
		if err != nil || task["enabled"] != true {
			continue
		}
		params, _ := task["parameters"].(map[string]any)
		var summary map[string]any
		_ = json.Unmarshal([]byte(r.summary), &summary)
		if summary == nil {
			summary = map[string]any{}
		}
		if r.status == "running" || r.status == "retry_wait" {
			scheduledAt, err := time.Parse(time.RFC3339Nano, r.scheduledAt)
			if err != nil || time.Since(scheduledAt) > 5*time.Minute {
				_, _ = s.DB.SQL.Exec("UPDATE task_runs SET status='missed',finished_at=? WHERE id=? AND status IN ('running','retry_wait')", time.Now().UTC().Format(time.RFC3339Nano), r.id)
				continue
			}
			var targets []string
			if r.status == "running" {
				raw, _ := json.Marshal(task["node_ids"])
				_ = json.Unmarshal(raw, &targets)
			} else {
				retryAt, _ := summary["retry_at"].(string)
				at, err := time.Parse(time.RFC3339Nano, retryAt)
				if err != nil || time.Now().UTC().Before(at) {
					continue
				}
				raw, _ := json.Marshal(summary["retry_nodes"])
				_ = json.Unmarshal(raw, &targets)
			}
			counts := map[string]int{}
			creationErrors := 0
			if r.status == "retry_wait" {
				if previous, ok := summary["creation_errors"].(float64); ok {
					creationErrors = int(previous)
				}
			}
			for _, nodeID := range targets {
				requestID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%s:%d", r.id, nodeID, r.attempt))).String()
				m, err := s.Actions.Create(ctx, nodeID, task["action"].(string), params, "scheduled", requestID, false, r.taskID, r.id)
				if err != nil {
					counts[err.Error()]++
					creationErrors++
				} else {
					counts[m["status"].(string)]++
				}
			}
			delete(summary, "retry_at")
			delete(summary, "retry_nodes")
			summary["counts"] = counts
			summary["creation_errors"] = creationErrors
			_, _ = s.DB.SQL.Exec("UPDATE task_runs SET status='dispatched',summary_json=? WHERE id=? AND status=?", db.JSON(summary), r.id, r.status)
			continue
		}
		ar, err := s.DB.SQL.Query(`SELECT a.node_id,a.status,a.error_code,a.result_json FROM action_requests a WHERE a.task_run_id=? AND a.rowid=(SELECT MAX(b.rowid) FROM action_requests b WHERE b.task_run_id=a.task_run_id AND b.node_id=a.node_id)`, r.id)
		if err != nil {
			continue
		}
		total, done, failed := 0, 0, 0
		var retryNodes []string
		for ar.Next() {
			var nodeID, st string
			var code, result sql.NullString
			if ar.Scan(&nodeID, &st, &code, &result) != nil {
				continue
			}
			total++
			switch st {
			case "succeeded":
				done++
			case "skipped_offline", "expired":
				done++
				failed++
				retryNodes = append(retryNodes, nodeID)
			case "failed":
				done++
				failed++
				var resultValue map[string]any
				if code.String == "ip_unchanged" && task["action"] == "change_ip" && json.Unmarshal([]byte(result.String), &resultValue) == nil && resultValue["final_warp_state"] == "on" {
					retryNodes = append(retryNodes, nodeID)
				}
			case "timed_out", "canceled":
				done++
				failed++
			}
		}
		ar.Close()
		if done < total {
			continue
		}
		if total == 0 {
			_, _ = s.DB.SQL.Exec("UPDATE task_runs SET status='failed',finished_at=? WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), r.id)
			continue
		}
		maxRetries, _ := task["max_retries"].(int)
		if len(retryNodes) > 0 && r.attempt < maxRetries {
			interval := 30
			if values, ok := task["retry_intervals_seconds"].([]any); ok && len(values) > r.attempt {
				if n, ok := values[r.attempt].(float64); ok {
					interval = int(n)
				}
			}
			summary["retry_at"] = time.Now().UTC().Add(time.Duration(interval) * time.Second).Format(time.RFC3339Nano)
			summary["retry_nodes"] = retryNodes
			_, _ = s.DB.SQL.Exec("UPDATE task_runs SET status='retry_wait',attempt=attempt+1,summary_json=?,finished_at=NULL WHERE id=? AND status='dispatched'", db.JSON(summary), r.id)
			continue
		}
		final := "succeeded"
		creationErrors, _ := summary["creation_errors"].(float64)
		if failed > 0 || creationErrors > 0 {
			final = "failed"
		}
		_, _ = s.DB.SQL.Exec("UPDATE task_runs SET status=?,summary_json=?,finished_at=? WHERE id=?", final, db.JSON(summary), time.Now().UTC().Format(time.RFC3339Nano), r.id)
	}
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
