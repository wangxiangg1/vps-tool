package nodes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"regexp"
	"strings"
	"time"
	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
)

var serviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,127}$`)

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

type Service struct {
	DB  *db.DB
	Cfg config.Settings
}

type scanner interface{ Scan(...any) error }

func nodeRow(r scanner) (map[string]any, error) {
	var id, name, region, tags, warp, xui, notes, warpStatus, xuiStatus, status, created, updated string
	var agent, host, osn, osv, arch, ip4, ip6, lastStatus, lastSeen, lastIP, lastResult sql.NullString
	var cpu, mu, mt, ru, rt, up sql.NullFloat64
	e := r.Scan(&id, &name, &region, &tags, &warp, &xui, &notes, &agent, &host, &osn, &osv, &arch, &cpu, &mu, &mt, &ru, &rt, &up, &warpStatus, &xuiStatus, &status, &ip4, &ip6, &lastStatus, &lastSeen, &lastIP, &lastResult, &created, &updated)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"id": id, "name": name, "region": region, "warp_adapter": warp, "xui_service": xui, "notes": notes, "created_at": created, "updated_at": updated, "warp_status": warpStatus, "xui_status": xuiStatus}
	var decodedTags, decodedStatus any
	json.Unmarshal([]byte(tags), &decodedTags)
	json.Unmarshal([]byte(status), &decodedStatus)
	out["tags"] = decodedTags
	out["status"] = decodedStatus
	for k, v := range map[string]sql.NullString{"agent_version": agent, "hostname": host, "os_name": osn, "os_version": osv, "architecture": arch, "public_ipv4": ip4, "public_ipv6": ip6, "last_status_at": lastStatus, "last_seen_at": lastSeen, "last_ip_change_at": lastIP} {
		if v.Valid {
			out[k] = v.String
		} else {
			out[k] = nil
		}
	}
	for k, v := range map[string]sql.NullFloat64{"cpu_percent": cpu, "memory_used_bytes": mu, "memory_total_bytes": mt, "root_used_bytes": ru, "root_total_bytes": rt, "uptime_seconds": up} {
		if v.Valid {
			out[k] = v.Float64
		} else {
			out[k] = nil
		}
	}
	if lastResult.Valid {
		var x any
		json.Unmarshal([]byte(lastResult.String), &x)
		out["last_ip_change_result"] = x
	} else {
		out["last_ip_change_result"] = nil
	}
	return out, nil
}

const selectNode = `SELECT id,name,region,tags_json,warp_adapter,xui_service,notes,agent_version,hostname,os_name,os_version,architecture,cpu_percent,memory_used_bytes,memory_total_bytes,root_used_bytes,root_total_bytes,uptime_seconds,warp_status,xui_status,status_json,public_ipv4,public_ipv6,last_status_at,last_seen_at,last_ip_change_at,last_ip_change_result_json,created_at,updated_at FROM nodes`

func (S *Service) Get(id string) (map[string]any, error) {
	return nodeRow(S.DB.SQL.QueryRow(selectNode+" WHERE id=? AND deleted_at IS NULL", id))
}
func (S *Service) List(online map[string]bool) []map[string]any {
	rows, e := S.DB.SQL.Query(selectNode + " WHERE deleted_at IS NULL ORDER BY name COLLATE NOCASE,id")
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		m, err := nodeRow(rows)
		if err != nil {
			continue
		}
		m["online"] = online[m["id"].(string)]
		out = append(out, m)
	}
	return out
}
func (S *Service) Create(name, region string, tags []string, warp, xui, notes, actor string) (map[string]any, string, string, error) {
	if name == "" || len([]rune(name)) > 100 || len([]rune(region)) > 100 || len(tags) > 20 || len([]rune(notes)) > 2000 || !serviceRE.MatchString(xui) || !validAdapter(warp) {
		return nil, "", "", &ValidationError{"invalid node parameters"}
	}
	if tags == nil {
		tags = []string{}
	}
	id, token := uuid.NewString(), auth.Token("enroll")
	now := time.Now().UTC()
	exp := now.Add(time.Duration(S.Cfg.EnrollmentTTL) * time.Second)
	e := S.immediate(func(conn *sql.Conn) error {
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, `INSERT INTO nodes(id,name,region,tags_json,warp_adapter,xui_service,notes,warp_status,xui_status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'unknown','unknown',?,?)`, id, name, region, db.JSON(tags), warp, xui, notes, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO enrollment_tokens(id,node_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?)`, uuid.NewString(), id, auth.HashToken(token), now.Format(time.RFC3339Nano), exp.Format(time.RFC3339Nano))
		return err
	})
	if e != nil {
		return nil, "", "", e
	}
	m, e := S.Get(id)
	return m, token, exp.Format(time.RFC3339Nano), e
}
func (S *Service) Enroll(id, token string) (map[string]any, string, error) {
	conn, err := S.DB.SQL.Conn(context.Background())
	if err != nil {
		return nil, "", err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return nil, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var h, exp string
	var used sql.NullString
	var tokenID string
	e := conn.QueryRowContext(context.Background(), `SELECT e.id,e.token_hash,e.expires_at,e.used_at FROM enrollment_tokens e JOIN nodes n ON n.id=e.node_id WHERE e.node_id=? AND e.token_hash=? AND n.deleted_at IS NULL`, id, auth.HashToken(token)).Scan(&tokenID, &h, &exp, &used)
	if e != nil || used.Valid || subtle.ConstantTimeCompare([]byte(auth.HashToken(token)), []byte(h)) != 1 {
		return nil, "", fmt.Errorf("registration denied")
	}
	t, parseErr := time.Parse(time.RFC3339Nano, exp)
	if parseErr != nil || !time.Now().UTC().Before(t) {
		return nil, "", fmt.Errorf("registration denied")
	}
	cred := auth.Token("agent")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, e = conn.ExecContext(context.Background(), "UPDATE enrollment_tokens SET used_at=? WHERE id=? AND used_at IS NULL", now, tokenID)
	if e == nil {
		_, e = conn.ExecContext(context.Background(), "UPDATE agent_credentials SET status='revoked',revoked_at=? WHERE node_id=? AND status='active'", now, id)
	}
	if e == nil {
		_, e = conn.ExecContext(context.Background(), "INSERT INTO agent_credentials(id,node_id,credential_hash,status,created_at) VALUES(?,?,?,'active',?)", uuid.NewString(), id, auth.HashToken(cred), now)
	}
	if e != nil {
		return nil, "", e
	}
	if _, e = conn.ExecContext(context.Background(), "COMMIT"); e != nil {
		return nil, "", e
	}
	committed = true
	_ = conn.Close()
	m, _ := S.Get(id)
	return m, cred, e
}

func (S *Service) NewEnrollment(id string) (string, string, error) {
	token := auth.Token("enroll")
	now := time.Now().UTC()
	exp := now.Add(time.Duration(S.Cfg.EnrollmentTTL) * time.Second)
	err := S.immediate(func(conn *sql.Conn) error {
		ctx := context.Background()
		var found string
		if err := conn.QueryRowContext(ctx, "SELECT id FROM nodes WHERE id=? AND deleted_at IS NULL", id).Scan(&found); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE enrollment_tokens SET used_at=? WHERE node_id=? AND used_at IS NULL", now.Format(time.RFC3339Nano), id); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO enrollment_tokens(id,node_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?)", uuid.NewString(), id, auth.HashToken(token), now.Format(time.RFC3339Nano), exp.Format(time.RFC3339Nano))
		return err
	})
	return token, exp.Format(time.RFC3339Nano), err
}
func (S *Service) AuthCredential(id, cred string) (map[string]any, bool) {
	var h string
	e := S.DB.SQL.QueryRow(`SELECT c.credential_hash FROM agent_credentials c JOIN nodes n ON n.id=c.node_id WHERE c.node_id=? AND c.status='active' AND n.deleted_at IS NULL`, id).Scan(&h)
	if e != nil {
		return nil, false
	}
	sum := sha256.Sum256([]byte(cred))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(h)) != 1 {
		return nil, false
	}
	m, e := S.Get(id)
	return m, e == nil
}
func (S *Service) Seen(id string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	S.DB.SQL.Exec("UPDATE nodes SET last_seen_at=?,updated_at=? WHERE id=?", now, now, id)
}
func (S *Service) UpdateStatus(id string, p map[string]any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	fields := []string{"agent_version", "hostname", "os_name", "os_version", "architecture", "cpu_percent", "memory_used_bytes", "memory_total_bytes", "root_used_bytes", "root_total_bytes", "uptime_seconds", "warp_status", "xui_status", "public_ipv4", "public_ipv6"}
	sets := make([]string, 0, len(fields)+4)
	args := make([]any, 0, len(fields)+5)
	for _, field := range fields {
		if value, ok := p[field]; ok && value != nil {
			sets = append(sets, field+"=?")
			args = append(args, value)
		}
	}
	observed, ok := p["observed_at"].(string)
	if !ok || observed == "" {
		observed = now
	}
	sets = append(sets, "status_json=?", "last_status_at=?", "last_seen_at=?", "updated_at=?")
	args = append(args, string(b), observed, now, now, id)
	_, err = S.DB.SQL.Exec("UPDATE nodes SET "+strings.Join(sets, ",")+" WHERE id=? AND deleted_at IS NULL", args...)
	return err
}
func (S *Service) Rotate(id string) (string, bool) {
	c := auth.Token("agent")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e := S.immediate(func(conn *sql.Conn) error {
		var found string
		if err := conn.QueryRowContext(context.Background(), "SELECT id FROM nodes WHERE id=? AND deleted_at IS NULL", id).Scan(&found); err != nil {
			return err
		}
		if _, err := conn.ExecContext(context.Background(), "UPDATE agent_credentials SET status='revoked',revoked_at=? WHERE node_id=? AND status='active'", now, id); err != nil {
			return err
		}
		_, err := conn.ExecContext(context.Background(), "INSERT INTO agent_credentials(id,node_id,credential_hash,status,created_at) VALUES(?,?,?,'active',?)", uuid.NewString(), id, auth.HashToken(c), now)
		return err
	})
	return c, e == nil
}
func (S *Service) Delete(id string) bool {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e := S.immediate(func(conn *sql.Conn) error {
		r, err := conn.ExecContext(context.Background(), "UPDATE nodes SET deleted_at=?,updated_at=? WHERE id=? AND deleted_at IS NULL", now, now, id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		_, err = conn.ExecContext(context.Background(), "UPDATE agent_credentials SET status='revoked',revoked_at=? WHERE node_id=? AND status='active'", now, id)
		return err
	})
	return e == nil
}

func (S *Service) Update(id string, fields map[string]any) (map[string]any, error) {
	allowed := map[string]string{"name": "name", "region": "region", "tags": "tags_json", "warp_adapter": "warp_adapter", "xui_service": "xui_service", "notes": "notes"}
	sets := []string{}
	args := []any{}
	for k, v := range fields {
		col, ok := allowed[k]
		if !ok {
			return nil, &ValidationError{"unsupported node field"}
		}
		if v == nil {
			continue
		}
		if col == "tags_json" {
			tags, ok := v.([]any)
			if !ok || len(tags) > 20 {
				return nil, &ValidationError{"invalid tags"}
			}
			for _, tag := range tags {
				if _, ok := tag.(string); !ok {
					return nil, &ValidationError{"invalid tags"}
				}
			}
			b, _ := json.Marshal(v)
			v = string(b)
		} else {
			text, ok := v.(string)
			if !ok {
				return nil, &ValidationError{"invalid node field"}
			}
			switch k {
			case "name":
				if text == "" || len([]rune(text)) > 100 {
					return nil, &ValidationError{"invalid node name"}
				}
			case "region":
				if len([]rune(text)) > 100 {
					return nil, &ValidationError{"invalid region"}
				}
			case "notes":
				if len([]rune(text)) > 2000 {
					return nil, &ValidationError{"invalid notes"}
				}
			case "warp_adapter":
				if !validAdapter(text) {
					return nil, &ValidationError{"invalid warp adapter"}
				}
			case "xui_service":
				if !serviceRE.MatchString(text) {
					return nil, &ValidationError{"invalid service name"}
				}
			}
		}
		sets = append(sets, col+"=?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		return S.Get(id)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sets = append(sets, "updated_at=?")
	args = append(args, now, id)
	r, e := S.DB.SQL.Exec("UPDATE nodes SET "+strings.Join(sets, ",")+" WHERE id=? AND deleted_at IS NULL", args...)
	if e != nil {
		return nil, e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	return S.Get(id)
}
func validAdapter(v string) bool { return v == "generic" || v == "warp-cli" || v == "wgcf" }
func (S *Service) Revoke(id string) bool {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e := S.immediate(func(conn *sql.Conn) error {
		var found string
		if err := conn.QueryRowContext(context.Background(), "SELECT id FROM nodes WHERE id=? AND deleted_at IS NULL", id).Scan(&found); err != nil {
			return err
		}
		_, err := conn.ExecContext(context.Background(), "UPDATE agent_credentials SET status='revoked',revoked_at=? WHERE node_id=? AND status='active'", now, id)
		return err
	})
	return e == nil
}

func (S *Service) immediate(fn func(*sql.Conn) error) error {
	ctx := context.Background()
	conn, err := S.DB.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
func (S *Service) History(id string) []map[string]any {
	rows, e := S.DB.SQL.Query("SELECT id,request_id,success,error_code,error_message,result_json,created_at FROM ip_change_history WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT 100", id)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var i, request, ec, em, r, c sql.NullString
		var ok bool
		if rows.Scan(&i, &request, &ok, &ec, &em, &r, &c) != nil {
			continue
		}
		var x map[string]any
		json.Unmarshal([]byte(r.String), &x)
		message, _ := x["message"].(string)
		if message == "" {
			message = em.String
		}
		out = append(out, map[string]any{"id": i.String, "request_id": request.String, "success": ok, "error_code": ec.String, "error_message": em.String, "old_ip": x["old_ip"], "new_ip": x["new_ip"], "attempts": x["attempts"], "final_warp_state": x["final_warp_state"], "duration_ms": x["duration_ms"], "message": message, "created_at": c.String})
	}
	return out
}
func (s *Service) ValidateXUI(v string) error {
	if !serviceRE.MatchString(strings.TrimSuffix(v, ".service")) {
		return fmt.Errorf("invalid xui_service")
	}
	return nil
}
