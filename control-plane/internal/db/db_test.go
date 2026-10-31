package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyHistoryAndConnectionSettings(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "legacy sample.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, version := range []int{1, 2} {
		if _, err := d.SQL.ExecContext(ctx, migrations[version]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO nodes(id,name,created_at,updated_at) VALUES('node','node','2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO action_requests(id,node_id,action,parameters_json,source,status,issued_at,deadline_at,created_at,updated_at,result_json) VALUES('request','node','change_ip','{}','manual','succeeded','2026-01-01','2026-01-02','2026-01-01','2026-01-01','{"new_ip":"192.0.2.1"}')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var version, count int
	if err := d.SQL.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d: %v", version, err)
	}
	if err := d.SQL.QueryRowContext(ctx, `SELECT count(*) FROM ip_change_history WHERE node_id='node' AND request_id='request' AND success=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history count=%d: %v", count, err)
	}

	connections := make([]*sql.Conn, 0, 4)
	defer func() {
		for _, conn := range connections {
			conn.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := d.SQL.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var foreignKeys, busyTimeout int
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 {
			t.Fatalf("connection %d: foreign_keys=%d busy_timeout=%d", i, foreignKeys, busyTimeout)
		}
	}
}
