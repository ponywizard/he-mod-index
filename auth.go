package main

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Anonymous bool   `json:"anonymous"`
}
type identity struct {
	User           *user
	Bearer         bool
	CSRF           string
	credentialHash string
}

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{2,31}$`)

func validateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return problem(400, "Username must be 3–32 letters, numbers, underscores, or hyphens.")
	}
	return nil
}
func validateAccount(name, password string) error {
	if err := validateUsername(name); err != nil {
		return err
	}
	if len(password) < 10 || len(password) > 128 {
		return problem(400, "Password must be 10–128 bytes.")
	}
	return nil
}
func (a *app) derive(password string, salt []byte) ([]byte, error) {
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		return nil, problem(503, "Server busy. Try again shortly.")
	}
	return pbkdf2.Key(sha256.New, password, salt, 600000, 32)
}
func (a *app) hashPassword(password string) (string, error) {
	salt := []byte(randomToken())
	key, err := a.derive(password, salt)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$600000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func (a *app) checkPassword(password, hash string) (bool, error) {
	p := strings.Split(hash, "$")
	salt := []byte("missing-account-dummy-salt")
	var want []byte
	if len(p) == 4 && p[0] == "pbkdf2-sha256" && p[1] == "600000" {
		salt, _ = base64.RawStdEncoding.DecodeString(p[2])
		want, _ = base64.RawStdEncoding.DecodeString(p[3])
	}
	got, err := a.derive(password, salt)
	return subtle.ConstantTimeCompare(got, want) == 1, err
}
func (a *app) provisionUser(name, password, role string) error {
	if err := validateAccount(name, password); err != nil {
		return err
	}
	if role != "user" && role != "reviewer" && role != "admin" {
		return errors.New("role must be user, reviewer, or admin")
	}
	hash, err := a.hashPassword(password)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRow(`INSERT INTO users(username,password,role,created_at) VALUES(?,?,?,?) ON CONFLICT(username) DO UPDATE SET password=excluded.password,role=excluded.role RETURNING id`, name, hash, role, time.Now().Unix()).Scan(&id); err != nil {
		return err
	}
	for _, table := range []string{"sessions", "tokens"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE user_id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) changeUserRole(name, role string) (bool, error) {
	if role != "user" && role != "reviewer" && role != "admin" {
		return false, errors.New("role must be user, reviewer, or admin")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	var current string
	if err = tx.QueryRow("SELECT id,role FROM users WHERE username=?", name).Scan(&id, &current); errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("user no longer exists")
	}
	if err != nil {
		return false, err
	}
	if current == role {
		return false, nil
	}
	if _, err = tx.Exec("UPDATE users SET role=? WHERE id=?", role, id); err != nil {
		return false, err
	}
	for _, table := range []string{"sessions", "tokens"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE user_id=?", id); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
func (a *app) authenticate(r *http.Request) (identity, error) {
	var i identity
	var hash string
	var query string
	if auth := r.Header.Get("Authorization"); auth != "" {
		if !strings.HasPrefix(auth, "Bearer ") {
			return i, problem(401, "Invalid authorization.")
		}
		i.Bearer = true
		hash = digest(strings.TrimPrefix(auth, "Bearer "))
		query = `SELECT u.id,u.username,u.role,u.anonymous FROM tokens t JOIN users u ON u.id=t.user_id WHERE t.hash=? AND t.expires_at>?`
	} else {
		c, err := r.Cookie("he_session")
		if err != nil {
			return i, nil
		}
		hash = digest(c.Value)
		i.CSRF = a.keyed("csrf:" + c.Value)
		query = `SELECT u.id,u.username,u.role,u.anonymous FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.expires_at>?`
	}
	u := new(user)
	err := a.db.QueryRow(query, hash, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role, &u.Anonymous)
	if errors.Is(err, sql.ErrNoRows) {
		if i.Bearer {
			return i, problem(401, "Token is invalid or expired.")
		}
		return identity{}, nil
	}
	if err != nil {
		return i, err
	}
	i.User = u
	i.credentialHash = hash
	return i, nil
}
func (a *app) require(r *http.Request, browserOnly bool) (identity, error) {
	i, err := a.authenticate(r)
	if err != nil {
		return i, err
	}
	if i.User == nil {
		return i, problem(401, "Sign in required.")
	}
	if browserOnly && i.Bearer {
		return i, problem(403, "Upload tokens can only manage mods and images.")
	}
	if !i.Bearer && r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(i.CSRF), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
		return i, problem(403, "Session changed. Reload the page.")
	}
	return i, nil
}

// Recheck the credential in the same transaction as its write. A request may
// have waited on its body, remote images, or hashing since require() succeeded.
func (a *app) beginWrite(i identity) (*sql.Tx, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	table := "sessions"
	if i.Bearer {
		table = "tokens"
	}
	var valid bool
	err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM "+table+" c JOIN users u ON u.id=c.user_id WHERE c.hash=? AND c.user_id=? AND c.expires_at>? AND u.role=?)", i.credentialHash, i.User.ID, time.Now().Unix(), i.User.Role).Scan(&valid)
	if err == nil && !valid {
		err = problem(401, "Session or token is no longer valid.")
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (a *app) setSession(w http.ResponseWriter, u *user, passwordHash string) error {
	raw := randomToken()
	expires := time.Now().Add(30 * 24 * time.Hour)
	// Password verification happens outside the DB transaction. Do not issue a
	// session if a reset or role change invalidated that verification meanwhile.
	res, err := a.db.Exec("INSERT INTO sessions(hash,user_id,expires_at) SELECT ?,id,? FROM users WHERE id=? AND password=? AND role=?", digest(raw), expires.Unix(), u.ID, passwordHash, u.Role)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return problem(401, "Account changed. Sign in again.")
	}
	http.SetCookie(w, &http.Cookie{Name: "he_session", Value: raw, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.cfg.PublicURL, "https:"), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 30 * 24 * 3600})
	return nil
}
func (a *app) session(w http.ResponseWriter, r *http.Request) error {
	i, err := a.authenticate(r)
	if err != nil {
		return err
	}
	if i.Bearer {
		return problem(403, "Use a browser session.")
	}
	return writeJSON(w, 200, map[string]any{"user": i.User, "csrf": i.CSRF})
}

func (a *app) updateSettings(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	var in struct {
		Anonymous *bool `json:"anonymous"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Anonymous == nil {
		return problem(400, "anonymous must be true or false.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE users SET anonymous=? WHERE id=?", *in.Anonymous, i.User.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return problem(401, "Sign in required.")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	i.User.Anonymous = *in.Anonymous
	return writeJSON(w, 200, i.User)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *app) register(w http.ResponseWriter, r *http.Request) error {
	ip := a.keyed("ip:" + a.clientIP(r))
	if err := a.limit("auth:"+ip, 20, 15*time.Minute); err != nil {
		return err
	}
	var in credentials
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := validateAccount(in.Username, in.Password); err != nil {
		return err
	}
	switch strings.ToLower(in.Username) {
	case "admin", "devanon":
		return problem(409, "This username is reserved.")
	}
	// Check the successful-registration cooldown before doing expensive password work.
	var expiry int64
	err := a.db.QueryRow("SELECT expires_at FROM limits WHERE key=?", "register:"+ip).Scan(&expiry)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if expiry > time.Now().Unix() {
		return &apiError{429, "Registration cooldown for this network.", int(expiry - time.Now().Unix())}
	}
	hash, err := a.hashPassword(in.Password)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	err = tx.QueryRow(`INSERT INTO limits(key,count,expires_at) VALUES(?,1,?) ON CONFLICT(key) DO UPDATE SET count=1,expires_at=excluded.expires_at WHERE limits.expires_at<=? RETURNING count`, "register:"+ip, time.Now().Add(a.cfg.RegistrationCooldown).Unix(), time.Now().Unix()).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return &apiError{429, "Registration cooldown for this network.", int(a.cfg.RegistrationCooldown.Seconds())}
	}
	if err != nil {
		return err
	}
	res, err := tx.Exec("INSERT INTO users(username,password,created_at) VALUES(?,?,?)", in.Username, hash, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return problem(409, "Username is already taken.")
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	u := &user{ID: id, Username: in.Username, Role: "user"}
	if err = a.setSession(w, u, hash); err != nil {
		return err
	}
	return writeJSON(w, 201, u)
}
func (a *app) login(w http.ResponseWriter, r *http.Request) error {
	ip := a.keyed("ip:" + a.clientIP(r))
	if err := a.limit("auth:"+ip, 20, 15*time.Minute); err != nil {
		return err
	}
	var in credentials
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if len(in.Username) > 32 || len(in.Password) > 128 {
		return problem(401, "Invalid username or password.")
	}
	if err := a.limit("login:"+a.keyed(strings.ToLower(in.Username)), 10, 15*time.Minute); err != nil {
		return err
	}
	u := new(user)
	var hash string
	err := a.db.QueryRow("SELECT id,username,role,password,anonymous FROM users WHERE username=?", in.Username).Scan(&u.ID, &u.Username, &u.Role, &hash, &u.Anonymous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ok, err := a.checkPassword(in.Password, hash)
	if err != nil {
		return err
	}
	if !ok {
		return problem(401, "Invalid username or password.")
	}
	if err = a.setSession(w, u, hash); err != nil {
		return err
	}
	return writeJSON(w, 200, u)
}
func (a *app) logout(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.require(r, true); err != nil {
		return err
	}
	c, _ := r.Cookie("he_session")
	if _, err := a.db.Exec("DELETE FROM sessions WHERE hash=?", digest(c.Value)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "he_session", Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.cfg.PublicURL, "https:"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	return writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *app) changePassword(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if err = a.limit("password:"+a.keyed(a.clientIP(r)), 10, 15*time.Minute); err != nil {
		return err
	}
	var in struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = validateAccount(i.User.Username, in.Password); err != nil {
		return err
	}
	if len(in.Current) > 128 {
		return problem(400, "Invalid current password.")
	}
	var old string
	if err = a.db.QueryRow("SELECT password FROM users WHERE id=?", i.User.ID).Scan(&old); err != nil {
		return err
	}
	ok, err := a.checkPassword(in.Current, old)
	if err != nil {
		return err
	}
	if !ok {
		return problem(403, "Invalid current password.")
	}
	hash, err := a.hashPassword(in.Password)
	if err != nil {
		return err
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE users SET password=? WHERE id=? AND password=?", hash, i.User.ID, old)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return problem(409, "Password changed. Sign in again.")
	}
	for _, table := range []string{"sessions", "tokens"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE user_id=?", i.User.ID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *app) listTokens(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	rows, err := a.db.Query("SELECT id,name,created_at,expires_at FROM tokens WHERE user_id=? AND expires_at>? ORDER BY id DESC", i.User.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, created, expires int64
		var name string
		if err = rows.Scan(&id, &name, &created, &expires); err != nil {
			return err
		}
		list = append(list, map[string]any{"id": id, "name": name, "created_at": created, "expires_at": expires})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}
func (a *app) createToken(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if err = a.limit("token:"+a.keyed(a.clientIP(r)), 10, time.Hour); err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 1 || len(in.Name) > 80 {
		return problem(400, "Token name must be 1–80 bytes.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM tokens WHERE user_id=? AND expires_at>?", i.User.ID, time.Now().Unix()).Scan(&count); err != nil {
		return err
	}
	if count >= 20 {
		return problem(400, "Revoke an existing token first (limit 20).")
	}
	raw := "he_" + randomToken()
	expires := time.Now().Add(365 * 24 * time.Hour).Unix()
	res, err := tx.Exec("INSERT INTO tokens(user_id,name,hash,created_at,expires_at) VALUES(?,?,?,?,?)", i.User.ID, in.Name, digest(raw), time.Now().Unix(), expires)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if err = tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"id": id, "token": raw, "expires_at": expires})
}
func (a *app) deleteToken(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM tokens WHERE id=? AND user_id=?", id, i.User.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return problem(404, "Token not found.")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]bool{"ok": true})
}
