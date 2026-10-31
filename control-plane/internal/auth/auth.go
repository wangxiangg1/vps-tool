package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"strings"
	"sync"
	"time"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
)

type Limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func NewLimiter() *Limiter { return &Limiter{failures: map[string][]time.Time{}} }
func (l *Limiter) Blocked(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a := l.failures[k]
	b := a[:0]
	for _, t := range a {
		if now.Sub(t) < 5*time.Minute {
			b = append(b, t)
		}
	}
	l.failures[k] = b
	return len(b) >= 5
}
func (l *Limiter) Fail(k string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[k] = append(l.failures[k], time.Now())
}
func (l *Limiter) Clear(k string) { l.mu.Lock(); defer l.mu.Unlock(); delete(l.failures, k) }
func Token(prefix string) string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
func HashToken(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func EqualToken(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
func EnsureAdmin(d *db.DB, s config.Settings) error {
	var id string
	e := d.SQL.QueryRow("SELECT id FROM users LIMIT 1").Scan(&id)
	if e == nil {
		return nil
	}
	if e != sql.ErrNoRows {
		return e
	}
	h, e := bcrypt.GenerateFromPassword([]byte(s.AdminPassword), bcrypt.DefaultCost)
	if e != nil {
		return e
	}
	n := time.Now().UTC().Format(time.RFC3339Nano)
	_, e = d.SQL.Exec("INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,?,?)", uuid.NewString(), s.AdminUser, string(h), n, n)
	return e
}

type Session struct{ ID, UserID, Username, CSRF, ExpiresAt string }

func VerifyPassword(v, h string) bool {
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(v)) == nil
}
func Login(d *db.DB, user, password string, ttlSeconds int) (map[string]any, string, string, string, error) {
	var id, hash string
	e := d.SQL.QueryRow("SELECT id,password_hash FROM users WHERE username=?", user).Scan(&id, &hash)
	if e != nil || !VerifyPassword(password, hash) {
		return nil, "", "", "", sql.ErrNoRows
	}
	st, csrf := Token("sess"), Token("csrf")
	now := time.Now().UTC()
	if ttlSeconds <= 0 {
		ttlSeconds = 28800
	}
	exp := now.Add(time.Duration(ttlSeconds) * time.Second).Format(time.RFC3339Nano)
	_, e = d.SQL.Exec("INSERT INTO sessions(id,user_id,session_hash,csrf_hash,created_at,expires_at,last_seen_at) VALUES(?,?,?,?,?,?,?)", uuid.NewString(), id, HashToken(st), HashToken(csrf), now.Format(time.RFC3339Nano), exp, now.Format(time.RFC3339Nano))
	return map[string]any{"id": id, "username": user}, st, csrf, exp, e
}
func Get(d *db.DB, token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	var x Session
	var ch string
	e := d.SQL.QueryRow("SELECT s.id,s.user_id,u.username,s.csrf_hash,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.session_hash=?", HashToken(token)).Scan(&x.ID, &x.UserID, &x.Username, &ch, &x.ExpiresAt)
	if e != nil {
		return x, false
	}
	t, e := time.Parse(time.RFC3339Nano, x.ExpiresAt)
	if e != nil || time.Now().UTC().After(t) {
		return Session{}, false
	}
	x.CSRF = ch
	return x, true
}

func RotateCSRF(d *db.DB, session Session) (string, bool) {
	raw := Token("csrf")
	_, err := d.SQL.Exec("UPDATE sessions SET csrf_hash=?,last_seen_at=? WHERE id=?", HashToken(raw), time.Now().UTC().Format(time.RFC3339Nano), session.ID)
	return raw, err == nil
}

func ChangePassword(d *db.DB, session Session, current, next string) error {
	var hash string
	if err := d.SQL.QueryRow("SELECT password_hash FROM users WHERE id=?", session.UserID).Scan(&hash); err != nil || !VerifyPassword(current, hash) {
		return fmt.Errorf("current_password_invalid")
	}
	if current == next {
		return fmt.Errorf("password_unchanged")
	}
	if len([]byte(next)) < 12 || len([]byte(next)) > 72 {
		return fmt.Errorf("password_length_invalid")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := d.SQL.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE users SET password_hash=?,updated_at=? WHERE id=?", string(h), time.Now().UTC().Format(time.RFC3339Nano), session.UserID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", session.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func Logout(d *db.DB, session Session) error {
	_, err := d.SQL.Exec("DELETE FROM sessions WHERE id=?", session.ID)
	return err
}
func Require(d *db.DB, s config.Settings, r *http.Request) (Session, int, string) {
	v, e := Get(d, cookie(r, s.CookieName))
	if !e {
		return Session{}, 401, "authentication_required"
	}
	return v, 0, ""
}
func RequireCSRF(d *db.DB, s config.Settings, r *http.Request) (Session, int, string) {
	v, c, m := Require(d, s, r)
	if c != 0 {
		return v, c, m
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(r.Header.Get("X-CSRF-Token"))), []byte(v.CSRF)) != 1 {
		return Session{}, 403, "csrf_invalid"
	}
	return v, 0, ""
}
func cookie(r *http.Request, n string) string {
	c, e := r.Cookie(n)
	if e != nil {
		return ""
	}
	return c.Value
}
func SetCookie(w http.ResponseWriter, s config.Settings, token string) {
	http.SetCookie(w, &http.Cookie{Name: s.CookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: s.SessionTTL})
}
func ClearCookie(w http.ResponseWriter, s config.Settings) {
	http.SetCookie(w, &http.Cookie{Name: s.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode})
}
func UserID(x Session) string { return strings.TrimSpace(x.UserID) }
