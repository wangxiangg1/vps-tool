package db

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestPythonDatabaseMigrationCompatibility(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	_, file, _, _ := runtime.Caller(0)
	serverDir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "server"))
	tmp := t.TempDir()
	legacyPath, referencePath, freshPath := filepath.Join(tmp, "legacy.db"), filepath.Join(tmp, "python.db"), filepath.Join(tmp, "go.db")
	code := `import sqlite3,sys
from pathlib import Path
from app.db import Database,SCHEMA_MIGRATIONS
legacy=sqlite3.connect(sys.argv[1])
for version in (1,2): legacy.executescript(SCHEMA_MIGRATIONS[version])
legacy.execute("PRAGMA user_version=2")
legacy.execute("INSERT INTO nodes(id,name,created_at,updated_at) VALUES('node','node','2026-01-01','2026-01-01')")
legacy.execute("INSERT INTO action_requests(id,node_id,action,parameters_json,source,status,issued_at,deadline_at,created_at,updated_at,result_json) VALUES('request','node','change_ip','{}','manual','succeeded','2026-01-01','2026-01-02','2026-01-01','2026-01-01','{}')")
legacy.commit()
legacy.close()
Database(Path(sys.argv[2])).initialize()`
	cmd := exec.Command(python, "-c", code, legacyPath, referencePath)
	for _, value := range os.Environ() {
		if len(value) >= 11 && value[:11] == "PYTHONPATH=" {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "PYTHONPATH="+serverDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create Python fixtures: %v\n%s", err, output)
	}
	ctx := context.Background()
	legacy, err := Open(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err = legacy.Init(ctx); err != nil {
		t.Fatalf("migrate Python v2: %v", err)
	}
	var count int
	if err = legacy.SQL.QueryRow("SELECT count(*) FROM ip_change_history WHERE request_id='request' AND success=1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("history count=%d: %v", count, err)
	}
	reference, err := Open(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	if err = reference.Init(ctx); err != nil {
		t.Fatalf("open Python v4: %v", err)
	}
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err = fresh.Init(ctx); err != nil {
		t.Fatalf("create Go v4: %v", err)
	}
	schema := func(database *DB) map[string][]string {
		t.Helper()
		objects, err := database.SQL.Query("SELECT type,name FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' ORDER BY type,name")
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		result := map[string][]string{}
		for objects.Next() {
			var kind, name string
			if err := objects.Scan(&kind, &name); err != nil {
				t.Fatal(err)
			}
			result[kind] = append(result[kind], name)
			if kind == "table" {
				tables = append(tables, name)
			}
		}
		objects.Close()
		for _, table := range tables {
			rows, err := database.SQL.Query("SELECT name,type,\"notnull\",pk FROM pragma_table_info(?) ORDER BY cid", table)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var name, kind string
				var required, primary int
				if err := rows.Scan(&name, &kind, &required, &primary); err != nil {
					t.Fatal(err)
				}
				result["columns:"+table] = append(result["columns:"+table], name+":"+kind+":"+string(rune('0'+required))+":"+string(rune('0'+primary)))
			}
			rows.Close()
		}
		return result
	}
	want := schema(reference)
	if got := schema(fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("Go schema differs from Python\nGo: %#v\nPython: %#v", got, want)
	}
	if got := schema(legacy); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from Python\nGo: %#v\nPython: %#v", got, want)
	}
}
