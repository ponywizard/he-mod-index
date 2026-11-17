package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed web/* docs/AUTHOR_AGENTS.md
var assets embed.FS

type app struct {
	db          *sql.DB
	cfg         config
	secret      string
	hashSlots   chan struct{}
	imageSlots  chan struct{}
	imageClient *http.Client
}
type apiError struct {
	status  int
	message string
	retry   int
}

func (e *apiError) Error() string              { return e.message }
func problem(status int, message string) error { return &apiError{status, message, 0} }

type endpoint func(http.ResponseWriter, *http.Request) error

func newApp(db *sql.DB, cfg config) (*app, error) {
	a := &app{db: db, cfg: cfg, hashSlots: make(chan struct{}, 4), imageSlots: make(chan struct{}, 4), imageClient: newImageClient()}
	if _, err := db.Exec("INSERT OR IGNORE INTO settings(key,value) VALUES('secret',?)", randomToken()); err != nil {
		return nil, err
	}
	if err := db.QueryRow("SELECT value FROM settings WHERE key='secret'").Scan(&a.secret); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "uploads"), 0700); err != nil {
		return nil, err
	}
	return a, a.backfillThumbnails()
}

func (a *app) keyed(value string) string {
	h := hmac.New(sha256.New, []byte(a.secret))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
func digest(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		return problem(415, "Use application/json.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return problem(400, "Invalid JSON body.")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return problem(400, "Expected one JSON object.")
	}
	return nil
}
func (a *app) handle(fn endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			var e *apiError
			if !errors.As(err, &e) {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
				e = &apiError{500, "Server error.", 0}
			}
			if e.retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(e.retry))
			}
			_ = writeJSON(w, e.status, map[string]string{"error": e.message})
		}
	}
}

func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/session", a.handle(a.session))
	m.HandleFunc("PUT /api/settings", a.handle(a.updateSettings))
	m.HandleFunc("POST /api/register", a.handle(a.register))
	m.HandleFunc("POST /api/login", a.handle(a.login))
	m.HandleFunc("POST /api/logout", a.handle(a.logout))
	m.HandleFunc("PUT /api/password", a.handle(a.changePassword))
	m.HandleFunc("GET /api/tokens", a.handle(a.listTokens))
	m.HandleFunc("POST /api/tokens", a.handle(a.createToken))
	m.HandleFunc("DELETE /api/tokens/{id}", a.handle(a.deleteToken))
	m.HandleFunc("GET /api/mods", a.handle(a.listMods))
	m.HandleFunc("GET /feed.xml", a.handle(a.feed))
	m.HandleFunc("GET /api/mods/{id}", a.handle(a.getMod))
	m.HandleFunc("POST /api/mods", a.handle(a.createMod))
	m.HandleFunc("PUT /api/mods/{id}", a.handle(a.updateMod))
	m.HandleFunc("DELETE /api/mods/{id}", a.handle(a.deleteMod))
	m.HandleFunc("PUT /api/mods/{id}/vote", a.handle(a.vote))
	m.HandleFunc("PUT /api/mods/{id}/approval", a.handle(a.approve))
	m.HandleFunc("POST /api/mods/{id}/image", a.handle(a.uploadImage))
	m.HandleFunc("POST /api/mods/{id}/image/crop", a.handle(a.recropImage))
	m.HandleFunc("POST /api/images/preview", a.handle(a.previewImage))
	m.HandleFunc("GET /api/admin/users", a.handle(a.listUsers))
	m.HandleFunc("DELETE /api/admin/users/{id}", a.handle(a.deleteUser))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.db.PingContext(r.Context()); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	m.HandleFunc("GET /AGENTS.md", func(w http.ResponseWriter, r *http.Request) {
		data, _ := assets.ReadFile("docs/AUTHOR_AGENTS.md")
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(data)
	})
	m.HandleFunc("GET /uploads/{name}", a.serveImage)
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		_ = writeJSON(w, 404, map[string]string{"error": "Not found."})
	})
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.NotFound(w, r)
			return
		}
		path := "web" + r.URL.Path
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/register" || r.URL.Path == "/account" || r.URL.Path == "/submit" || r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/mod/") {
			path = "web/index.html"
		}
		if path != "web/index.html" && path != "web/app.js" && path != "web/editor.js" && path != "web/style.css" && path != "web/default-mod.png" {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(path)))
		w.Write(data)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" {
			if (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != a.cfg.PublicURL) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				_ = writeJSON(w, 403, map[string]string{"error": "Origin rejected."})
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
