package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 4

type DB struct{ SQL *sql.DB }

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	if !strings.HasPrefix(uri.Path, "/") {
		uri.Path = "/" + uri.Path
	}
	params := url.Values{}
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "synchronous(NORMAL)")
	uri.RawQuery = params.Encode()
	x, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	x.SetMaxOpenConns(4)
	x.SetMaxIdleConns(2)
	if _, err = x.Exec("PRAGMA journal_mode=WAL"); err != nil {
		x.Close()
		return nil, err
	}
	return &DB{x}, nil
}
func (d *DB) Close() error { return d.SQL.Close() }
func (d *DB) Init(ctx context.Context) error {
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var v int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > SchemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", v, SchemaVersion)
	}
	for i := v + 1; i <= SchemaVersion; i++ {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, migrations[i]); err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			return err
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", i)); err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			return err
		}
	}
	return nil
}
func (d *DB) Exec(ctx context.Context, q string, args ...any) error {
	_, e := d.SQL.ExecContext(ctx, q, args...)
	return e
}
func now() string       { return time.Now().UTC().Format(time.RFC3339Nano) }
func JSON(v any) string { b, _ := json.Marshal(v); return string(b) }

var migrations = map[int]string{
	1: `CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE,password_hash TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,session_hash TEXT NOT NULL UNIQUE,csrf_hash TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,last_seen_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,name TEXT NOT NULL,region TEXT NOT NULL DEFAULT '',tags_json TEXT NOT NULL DEFAULT '[]',warp_adapter TEXT NOT NULL DEFAULT 'generic',xui_service TEXT NOT NULL DEFAULT 'x-ui',notes TEXT NOT NULL DEFAULT '',agent_version TEXT,hostname TEXT,os_name TEXT,os_version TEXT,architecture TEXT,cpu_percent REAL,memory_used_bytes INTEGER,memory_total_bytes INTEGER,root_used_bytes INTEGER,root_total_bytes INTEGER,uptime_seconds INTEGER,warp_status TEXT NOT NULL DEFAULT 'unknown',xui_status TEXT NOT NULL DEFAULT 'unknown',public_ipv4 TEXT,public_ipv6 TEXT,status_json TEXT NOT NULL DEFAULT '{}',last_status_at TEXT,last_seen_at TEXT,last_ip_change_at TEXT,last_ip_change_result_json TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,deleted_at TEXT); CREATE INDEX IF NOT EXISTS idx_nodes_deleted ON nodes(deleted_at); CREATE INDEX IF NOT EXISTS idx_nodes_last_seen ON nodes(last_seen_at);
CREATE TABLE IF NOT EXISTS agent_credentials(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,credential_hash TEXT NOT NULL UNIQUE,status TEXT NOT NULL CHECK(status IN ('active','revoked')),created_at TEXT NOT NULL,revoked_at TEXT); CREATE INDEX IF NOT EXISTS idx_agent_credentials_node ON agent_credentials(node_id,status);
CREATE TABLE IF NOT EXISTS enrollment_tokens(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,token_hash TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,used_at TEXT); CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_node ON enrollment_tokens(node_id,expires_at);
CREATE TABLE IF NOT EXISTS batches(id TEXT PRIMARY KEY,action TEXT NOT NULL,created_at TEXT NOT NULL,created_by TEXT REFERENCES users(id),status TEXT NOT NULL DEFAULT 'queued');
CREATE TABLE IF NOT EXISTS action_requests(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id),batch_id TEXT REFERENCES batches(id),task_id TEXT,task_run_id TEXT,action TEXT NOT NULL,parameters_json TEXT NOT NULL,source TEXT NOT NULL CHECK(source IN ('manual','batch','scheduled')),status TEXT NOT NULL,error_code TEXT,error_message TEXT,result_json TEXT,attempts INTEGER NOT NULL DEFAULT 0,issued_at TEXT NOT NULL,deadline_at TEXT NOT NULL,dispatched_at TEXT,accepted_at TEXT,started_at TEXT,finished_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_action_requests_node_status ON action_requests(node_id,status); CREATE INDEX IF NOT EXISTS idx_action_requests_created ON action_requests(created_at);
CREATE INDEX IF NOT EXISTS idx_action_requests_status_deadline ON action_requests(status,deadline_at);
CREATE TABLE IF NOT EXISTS action_results(id TEXT PRIMARY KEY,request_id TEXT NOT NULL REFERENCES action_requests(id) ON DELETE CASCADE,node_id TEXT NOT NULL REFERENCES nodes(id),success INTEGER NOT NULL,error_code TEXT,error_message TEXT,result_json TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_action_results_request ON action_results(request_id,created_at);
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,name TEXT NOT NULL,node_ids_json TEXT NOT NULL,action TEXT NOT NULL,parameters_json TEXT NOT NULL,schedule_type TEXT NOT NULL CHECK(schedule_type IN ('daily','weekly','cron')),schedule_value TEXT NOT NULL,timezone TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,max_retries INTEGER NOT NULL DEFAULT 2,retry_intervals_json TEXT NOT NULL DEFAULT '[30,90]',next_run_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,deleted_at TEXT); CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(enabled,next_run_at);
CREATE TABLE IF NOT EXISTS task_runs(id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,scheduled_for TEXT NOT NULL,status TEXT NOT NULL,attempt INTEGER NOT NULL DEFAULT 0,summary_json TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL,started_at TEXT,finished_at TEXT,UNIQUE(task_id,scheduled_for)); CREATE INDEX IF NOT EXISTS idx_task_runs_task ON task_runs(task_id,created_at);
CREATE TABLE IF NOT EXISTS audit_logs(id TEXT PRIMARY KEY,actor_type TEXT NOT NULL,actor_id TEXT,event_type TEXT NOT NULL,node_id TEXT,metadata_json TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at); CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value_json TEXT NOT NULL,updated_at TEXT NOT NULL);`,
	2: `CREATE TABLE IF NOT EXISTS scheduler_leases(name TEXT PRIMARY KEY,owner_id TEXT NOT NULL,lease_until TEXT NOT NULL,updated_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_scheduler_leases_until ON scheduler_leases(lease_until);`,
	3: `CREATE TABLE IF NOT EXISTS ip_change_history(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,request_id TEXT NOT NULL UNIQUE REFERENCES action_requests(id) ON DELETE CASCADE,success INTEGER NOT NULL CHECK(success IN (0,1)),error_code TEXT,error_message TEXT,result_json TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_ip_change_history_node_created ON ip_change_history(node_id,created_at DESC);
INSERT OR IGNORE INTO ip_change_history(id,node_id,request_id,success,error_code,error_message,result_json,created_at)
SELECT id,node_id,id,CASE WHEN status='succeeded' THEN 1 ELSE 0 END,error_code,error_message,COALESCE(result_json,'{}'),COALESCE(finished_at,updated_at,created_at) FROM action_requests WHERE action='change_ip' AND status IN ('succeeded','failed');
DELETE FROM ip_change_history WHERE id IN (SELECT id FROM (SELECT id,ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY created_at DESC,id DESC) AS row_number FROM ip_change_history) WHERE row_number>100);`,
	4: `CREATE TABLE IF NOT EXISTS xui_backups(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,filename TEXT NOT NULL,relative_path TEXT NOT NULL UNIQUE,size_bytes INTEGER NOT NULL DEFAULT 0,sha256 TEXT,status TEXT NOT NULL CHECK(status IN ('pending','ready')),created_at TEXT NOT NULL,updated_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS idx_xui_backups_node_created ON xui_backups(node_id,created_at DESC);`,
}
