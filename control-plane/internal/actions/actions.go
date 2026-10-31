package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"math"
	"net"
	"regexp"
	"time"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/models"
	"vps-tool/control-plane/internal/nodes"
)

type Gateway interface {
	Online(string) bool
	Send(context.Context, map[string]any) error
}
type Service struct {
	DB    *db.DB
	Cfg   config.Settings
	Nodes *nodes.Service
	GW    Gateway
}

func (s *Service) Validate(a string, p map[string]any) error {
	ok := false
	for _, x := range models.Actions {
		if x == a {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("unsupported_action")
	}
	if p == nil {
		return fmt.Errorf("invalid_parameters")
	}
	if a == "change_ip" {
		for k := range p {
			if k != "max_attempts" && k != "timeout_seconds" {
				return fmt.Errorf("invalid_parameters")
			}
		}
		if v, yes := p["max_attempts"]; yes {
			n, ok := v.(float64)
			if !ok || n != math.Trunc(n) || n < 1 || n > 3 {
				return fmt.Errorf("invalid_parameters")
			}
		}
		if v, yes := p["timeout_seconds"]; yes {
			n, ok := v.(float64)
			if !ok || n != math.Trunc(n) || n < 30 || n > 180 {
				return fmt.Errorf("invalid_parameters")
			}
		}
	}
	if a == "backup_xui" || a == "restore_xui" {
		id, ok := p["backup_id"].(string)
		if !ok || len(p) != 1 || !regexp.MustCompile(`^[A-Za-z0-9-]{36,64}$`).MatchString(id) {
			return fmt.Errorf("invalid_parameters")
		}
	}
	if a != "change_ip" && a != "backup_xui" && a != "restore_xui" && len(p) != 0 {
		return fmt.Errorf("invalid_parameters")
	}
	return nil
}
func (s *Service) Create(ctx context.Context, node, action string, p map[string]any, source, request string, queue bool, taskRefs ...string) (map[string]any, error) {
	if e := s.Validate(action, p); e != nil {
		return nil, e
	}
	if _, e := s.Nodes.Get(node); e != nil {
		return nil, fmt.Errorf("node_not_found")
	}
	if request == "" {
		request = uuid.NewString()
	}
	now := time.Now().UTC()
	status := "skipped_offline"
	var ec, em any = "node_offline", "Agent is offline; request was not queued"
	if (s.GW != nil && s.GW.Online(node)) || queue {
		status = "queued"
		ec = nil
		em = nil
	}
	ttl := s.Cfg.ActionTTL
	if (action == "install_warp" || action == "install_xui" || action == "backup_xui" || action == "restore_xui") && ttl < 1800 {
		ttl = 1800
	}
	deadline := now.Add(time.Duration(ttl) * time.Second)
	conn, err := s.DB.SQL.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var existingNode, existingAction, existingParams string
	err = conn.QueryRowContext(ctx, "SELECT node_id,action,parameters_json FROM action_requests WHERE id=?", request).Scan(&existingNode, &existingAction, &existingParams)
	if err == nil {
		var existing any
		if json.Unmarshal([]byte(existingParams), &existing) != nil || existingNode != node || existingAction != action || db.JSON(existing) != db.JSON(p) {
			return nil, fmt.Errorf("request_duplicate")
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		_ = conn.Close()
		return s.Get(request)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if models.StateChanging[action] {
		var n int
		e := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_requests WHERE node_id=? AND action IN ('warp_on','warp_off','change_ip','restart_xui','upgrade_agent','install_warp','install_xui','backup_xui','restore_xui') AND status IN ('queued','dispatched','accepted','running','unknown')`, node).Scan(&n)
		if e != nil {
			return nil, e
		}
		if n > 0 {
			return nil, fmt.Errorf("action_busy")
		}
	}
	var taskID, runID any
	if len(taskRefs) > 0 {
		taskID = taskRefs[0]
	}
	if len(taskRefs) > 1 {
		runID = taskRefs[1]
	}
	_, e := conn.ExecContext(ctx, `INSERT INTO action_requests(id,node_id,task_id,task_run_id,action,parameters_json,source,status,error_code,error_message,attempts,issued_at,deadline_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,0,?,?,?,?)`, request, node, taskID, runID, action, db.JSON(p), source, status, ec, em, now.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if e != nil {
		return nil, e
	}
	if _, e = conn.ExecContext(ctx, "COMMIT"); e != nil {
		return nil, e
	}
	committed = true
	_ = conn.Close()
	out, e := s.Get(request)
	if status == "queued" && s.GW != nil && s.GW.Online(node) {
		go s.Dispatch(context.Background(), request)
	}
	return out, e
}

type scanner interface{ Scan(...any) error }

const selectRequest = `SELECT id,node_id,batch_id,task_id,task_run_id,action,parameters_json,source,status,error_code,error_message,result_json,attempts,issued_at,deadline_at,dispatched_at,accepted_at,started_at,finished_at,created_at,updated_at FROM action_requests`

func scanRequest(row scanner) (map[string]any, error) {
	var id, n, batch, task, run, a, p, src, st, ec, em, res, iss, dl, dis, acc, sta, fin, cr, up sql.NullString
	var att int
	e := row.Scan(&id, &n, &batch, &task, &run, &a, &p, &src, &st, &ec, &em, &res, &att, &iss, &dl, &dis, &acc, &sta, &fin, &cr, &up)
	if e != nil {
		return nil, e
	}
	var pp, rr any
	json.Unmarshal([]byte(p.String), &pp)
	if res.Valid {
		json.Unmarshal([]byte(res.String), &rr)
	}
	value := func(v sql.NullString) any {
		if v.Valid {
			return v.String
		}
		return nil
	}
	return map[string]any{"id": id.String, "node_id": n.String, "batch_id": value(batch), "task_id": value(task), "task_run_id": value(run), "action": a.String, "parameters": pp, "source": src.String, "status": st.String, "error_code": value(ec), "error_message": value(em), "result": rr, "attempts": att, "issued_at": iss.String, "deadline_at": dl.String, "dispatched_at": value(dis), "accepted_at": value(acc), "started_at": value(sta), "finished_at": value(fin), "created_at": cr.String, "updated_at": up.String}, nil
}
func (s *Service) Get(id string) (map[string]any, error) {
	return scanRequest(s.DB.SQL.QueryRow(selectRequest+" WHERE id=?", id))
}
func (s *Service) List(node string) []map[string]any {
	q := selectRequest
	var a []any
	if node != "" {
		q += " WHERE node_id=?"
		a = append(a, node)
	}
	q += " ORDER BY created_at DESC LIMIT 100"
	rows, e := s.DB.SQL.Query(q, a...)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if m, e := scanRequest(rows); e == nil {
			out = append(out, m)
		}
	}
	return out
}
func (s *Service) Dispatch(ctx context.Context, id string) error {
	m, e := s.Get(id)
	if e != nil {
		return e
	}
	if s.GW == nil || !s.GW.Online(m["node_id"].(string)) {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r, err := s.DB.SQL.Exec("UPDATE action_requests SET status='dispatched',attempts=attempts+1,dispatched_at=COALESCE(dispatched_at,?),updated_at=? WHERE id=? AND status='queued' AND julianday(deadline_at)>julianday(?)", now, now, id, now)
	if err != nil {
		return err
	}
	changed, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return nil
	}
	if err := s.GW.Send(ctx, m); err != nil {
		_, _ = s.DB.SQL.Exec("UPDATE action_requests SET status='unknown',error_code='agent_disconnect',updated_at=? WHERE id=? AND status='dispatched'", now, id)
		return err
	}
	return nil
}

func (s *Service) DispatchPending(ctx context.Context, node string) error {
	rows, err := s.DB.SQL.QueryContext(ctx, "SELECT id FROM action_requests WHERE node_id=? AND status='queued' ORDER BY created_at,id LIMIT 100", node)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.Dispatch(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) MarkUnknownForNode(node string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.DB.SQL.Exec(`UPDATE action_requests SET status='unknown',error_code='result_unknown',error_message='Agent connection closed before a final result',updated_at=? WHERE node_id=? AND status IN ('dispatched','accepted','running')`, now, node)
	return err
}

func (s *Service) Accept(node, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.DB.SQL.Exec(`UPDATE action_requests SET status='accepted',accepted_at=COALESCE(accepted_at,?),error_code=NULL,error_message=NULL,updated_at=? WHERE id=? AND node_id=? AND status IN ('dispatched','unknown')`, now, now, id, node)
	return err
}

func (s *Service) Progress(node, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.DB.SQL.Exec(`UPDATE action_requests SET status='running',started_at=COALESCE(started_at,?),updated_at=? WHERE id=? AND node_id=? AND status IN ('dispatched','accepted','unknown')`, now, now, id, node)
	return err
}

func (s *Service) Finish(node, id string, ok bool, result map[string]any, code, msg string) error {
	if result == nil {
		result = map[string]any{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(b) > 64*1024 {
		result = map[string]any{"error": "result_too_large"}
		b, _ = json.Marshal(result)
		ok, code, msg = false, "result_too_large", "Agent result exceeded the 64 KiB limit"
	}
	st := "failed"
	var errorCode, errorMessage any = code, msg
	if ok {
		st = "succeeded"
		errorCode, errorMessage = nil, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.DB.SQL.Conn(ctx)
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
	var action, status string
	err = conn.QueryRowContext(ctx, "SELECT action,status FROM action_requests WHERE id=? AND node_id=?", id, node).Scan(&action, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch status {
	case "succeeded", "failed", "timed_out", "expired", "skipped_offline", "canceled":
		return nil
	}
	if _, err = conn.ExecContext(ctx, `UPDATE action_requests SET status=?,error_code=?,error_message=?,result_json=?,finished_at=?,updated_at=? WHERE id=? AND node_id=?`, st, errorCode, errorMessage, string(b), now, now, id, node); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO action_results(id,request_id,node_id,success,error_code,error_message,result_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, uuid.NewString(), id, node, ok, errorCode, errorMessage, string(b), now); err != nil {
		return err
	}
	if action == "change_ip" {
		if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO ip_change_history(id,node_id,request_id,success,error_code,error_message,result_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, uuid.NewString(), node, id, ok, errorCode, errorMessage, string(b), now); err != nil {
			return err
		}
		summary := make(map[string]any, len(result)+1)
		for key, value := range result {
			summary[key] = value
		}
		summary["success"] = ok
		query := "UPDATE nodes SET last_ip_change_at=?,last_ip_change_result_json=?,updated_at=?"
		args := []any{now, db.JSON(summary), now}
		if ipValue, exists := result["new_ip"].(string); ok && exists {
			if ip := net.ParseIP(ipValue); ip != nil {
				column := "public_ipv6"
				if ip.To4() != nil {
					column = "public_ipv4"
				}
				query += "," + column + "=?"
				args = append(args, ipValue)
			}
		}
		query += " WHERE id=? AND deleted_at IS NULL"
		args = append(args, node)
		if _, err = conn.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `DELETE FROM ip_change_history WHERE node_id=? AND id NOT IN (SELECT id FROM ip_change_history WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT 100)`, node, node); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
func (s *Service) Expire() {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.DB.SQL.Exec("UPDATE action_requests SET status='expired',error_code='request_expired',finished_at=?,updated_at=? WHERE status='queued' AND julianday(deadline_at)<=julianday(?)", now, now, now)
	s.DB.SQL.Exec("UPDATE action_requests SET status='timed_out',error_code='action_timeout',finished_at=?,updated_at=? WHERE status IN ('dispatched','accepted','running') AND julianday(deadline_at)<=julianday(?)", now, now, now)
}
