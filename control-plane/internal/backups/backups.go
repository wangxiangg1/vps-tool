package backups

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/nodes"
)

const MaxBytes int64 = 32 * 1024 * 1024

type Service struct {
	DB    *db.DB
	Cfg   config.Settings
	Nodes *nodes.Service
}

func (s *Service) Prepare(id string) (map[string]any, error) {
	n, e := s.Nodes.Get(id)
	if e != nil {
		return nil, fmt.Errorf("node_not_found")
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, n["name"].(string))
	svc := n["xui_service"].(string)
	suffix := "_xui.db"
	if strings.TrimSuffix(svc, ".service") == "xray" {
		suffix = "_xray-config.tar.gz"
	}
	id2 := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	safe = strings.Trim(safe, ".-_")
	if safe == "" {
		safe = "node"
	}
	if len(safe) > 80 {
		safe = safe[:80]
	}
	fn := safe + "_" + strings.ReplaceAll(strings.TrimSuffix(now, "Z"), ":", "") + suffix
	rel := filepath.Join(id, id2+".db")
	_, e = s.DB.SQL.Exec("INSERT INTO xui_backups(id,node_id,filename,relative_path,status,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?)", id2, id, fn, rel, now, now)
	if e != nil {
		return nil, e
	}
	return s.row(id2, id)
}
func (s *Service) List(id string) []map[string]any {
	rows, e := s.DB.SQL.Query("SELECT id,node_id,filename,relative_path,size_bytes,sha256,status,created_at,updated_at FROM xui_backups WHERE node_id=? ORDER BY created_at DESC LIMIT 50", id)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var i, n, f, r, st, cr, up string
		var sh sql.NullString
		var sz int64
		if rows.Scan(&i, &n, &f, &r, &sz, &sh, &st, &cr, &up) == nil {
			var digest any
			if sh.Valid {
				digest = sh.String
			}
			out = append(out, map[string]any{"id": i, "node_id": n, "filename": f, "relative_path": r, "size_bytes": sz, "sha256": digest, "status": st, "created_at": cr, "updated_at": up})
		}
	}
	return out
}
func (s *Service) row(id, node string) (map[string]any, error) {
	var n, f, r, st, cr, up string
	var sh sql.NullString
	var size int64
	e := s.DB.SQL.QueryRow("SELECT node_id,filename,relative_path,size_bytes,sha256,status,created_at,updated_at FROM xui_backups WHERE id=? AND node_id=?", id, node).Scan(&n, &f, &r, &size, &sh, &st, &cr, &up)
	if e != nil {
		return nil, e
	}
	var digest any
	if sh.Valid {
		digest = sh.String
	}
	return map[string]any{"id": id, "node_id": n, "filename": f, "relative_path": r, "size_bytes": size, "sha256": digest, "status": st, "created_at": cr, "updated_at": up}, nil
}

func (s *Service) Get(id, node string) (map[string]any, error) { return s.row(id, node) }
func (s *Service) path(m map[string]any) string {
	root, err := filepath.Abs(s.Cfg.BackupDir)
	if err != nil {
		return ""
	}
	p, err := filepath.Abs(filepath.Join(root, m["relative_path"].(string)))
	if err != nil {
		return ""
	}
	root, err = resolveExisting(root)
	if err != nil {
		return ""
	}
	p, err = resolveExisting(p)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return ""
	}
	return p
}

// Resolve existing symlinks even when the final file or directory is new.
func resolveExisting(path string) (string, error) {
	var missing []string
	for {
		_, err := os.Lstat(path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return resolved, nil
}
func (s *Service) Save(id, node string, r io.Reader) (map[string]any, error) {
	m, e := s.row(id, node)
	if e != nil {
		return nil, fmt.Errorf("backup_not_found")
	}
	p := s.path(m)
	if p == "" {
		return nil, fmt.Errorf("backup_path_invalid")
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return nil, e
	}
	p = s.path(m)
	if p == "" {
		return nil, fmt.Errorf("backup_path_invalid")
	}
	tmp := p + "." + uuid.NewString() + ".part"
	f, e := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, e
	}
	defer os.Remove(tmp)
	h := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if size == 0 || size > MaxBytes {
		return nil, fmt.Errorf("backup_too_large")
	}
	if e = os.Rename(tmp, p); e != nil {
		return nil, e
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, e = s.DB.SQL.Exec("UPDATE xui_backups SET size_bytes=?,sha256=?,status='ready',updated_at=? WHERE id=? AND node_id=?", size, hex.EncodeToString(h.Sum(nil)), now, id, node)
	if e != nil {
		return nil, e
	}
	return s.row(id, node)
}
func (s *Service) Ready(id, node string) (map[string]any, string, error) {
	m, e := s.row(id, node)
	if e != nil || m["status"] != "ready" {
		return nil, "", fmt.Errorf("backup_not_ready")
	}
	p := s.path(m)
	if p == "" {
		return nil, "", fmt.Errorf("backup_path_invalid")
	}
	if _, e = os.Stat(p); e != nil {
		return nil, "", fmt.Errorf("backup_missing")
	}
	return m, p, nil
}
func (s *Service) Delete(id, node string) bool {
	m, e := s.row(id, node)
	if e != nil {
		return false
	}
	p := s.path(m)
	if p == "" {
		return false
	}
	s.DB.SQL.Exec("DELETE FROM xui_backups WHERE id=? AND node_id=?", id, node)
	os.Remove(p)
	return true
}
