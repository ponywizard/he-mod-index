package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Model a client that sends headers and delays the request body. All requests
// use the real router, auth checks and temporary SQLite database.
type delayedRequestBody struct {
	reader  io.Reader
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (b *delayedRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.proceed
	return b.reader.Read(p)
}
func (b *delayedRequestBody) Close() error { return nil }

func pendingRequest(t *testing.T, c *testClient, method, path, content string, data []byte) func() *httptest.ResponseRecorder {
	t.Helper()
	body := &delayedRequestBody{reader: bytes.NewReader(data), started: make(chan struct{}), proceed: make(chan struct{})}
	r := httptest.NewRequest(method, path, nil)
	r.Body = body
	r.RemoteAddr = c.ip + ":12345"
	r.Header.Set("Content-Type", content)
	if c.cookie != nil {
		r.AddCookie(c.cookie)
		r.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { c.a.routes().ServeHTTP(w, r); close(done) }()
	var resume sync.Once
	release := func() { resume.Do(func() { close(body.proceed) }) }
	t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("pending request did not stop")
		}
	})
	select {
	case <-body.started:
	case <-done:
		t.Fatalf("request ended before reading body: %d %s", w.Code, w.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach body read")
	}
	return func() *httptest.ResponseRecorder {
		release()
		select {
		case <-done:
			return w
		case <-time.After(5 * time.Second):
			t.Fatal("pending request did not finish")
			return nil
		}
	}
}

func TestPendingImageCannotChangeReplacementMod(t *testing.T) {
	a := testApp(t)
	attacker, victim := client(a, "192.0.2.1"), client(a, "192.0.2.2")
	attacker.register(t, "attacker")
	v := victim.register(t, "victim")
	m := attacker.addMod(t, "attacker mod")
	finish := pendingRequest(t, attacker, "POST", modPath(m)+"/image", "image/png", splitImage(t))
	status(t, attacker.request("DELETE", modPath(m), nil, nil), 200)
	replacement := victim.addMod(t, "victim mod")
	if replacement.ID <= m.ID {
		t.Fatal("deleted mod ID was reused")
	}
	status(t, finish(), 409)
	current := readJSON[mod](t, victim.request("GET", modPath(replacement), nil, nil))
	if current.UserID != v.ID || current.ImageURL != "" {
		t.Fatal("pending image changed replacement mod")
	}
	files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
	if err != nil || len(files) != 0 {
		t.Fatal("rejected upload left image files", files, err)
	}
}

func TestStaleApprovalCannotApproveReplacementMod(t *testing.T) {
	a := testApp(t)
	owner, reviewer := client(a, "192.0.2.1"), client(a, "192.0.2.2")
	owner.register(t, "owner")
	reviewer.provision(t, "reviewer", "reviewer")
	original := owner.addMod(t, "safe release")
	viewed := readJSON[mod](t, reviewer.request("GET", modPath(original), nil, nil))
	status(t, owner.request("DELETE", modPath(original), nil, nil), 200)
	in := sampleMod("unreviewed replacement")
	in.DownloadURL = "https://example.org/unreviewed.pck"
	w := owner.request("POST", "/api/mods", in, nil)
	status(t, w, 201)
	replacement := readJSON[mod](t, w)
	if replacement.ID <= viewed.ID {
		t.Fatal("deleted mod ID was reused")
	}
	w = reviewer.request("PUT", modPath(viewed)+"/approval", map[string]any{"approved": true, "revision": viewed.Revision}, nil)
	status(t, w, 409)
	approved := readJSON[mod](t, owner.request("GET", modPath(replacement), nil, nil))
	if approved.Approval != "none" || approved.DownloadURL != in.DownloadURL {
		t.Fatal("stale review approved replacement release")
	}
}

func TestPendingTokenCreationRejectsRevocation(t *testing.T) {
	for _, action := range []string{"password", "logout", "role", "operator reset", "expiry", "deletion"} {
		t.Run(action, func(t *testing.T) {
			a := testApp(t)
			admin := client(a, "192.0.2.1")
			admin.provision(t, "operator", "admin")
			owner := client(a, "192.0.2.2")
			u := owner.register(t, "owner")
			finish := pendingRequest(t, owner, "POST", "/api/tokens", "application/json", []byte(`{"name":"pending token"}`))
			switch action {
			case "password":
				status(t, owner.request("PUT", "/api/password", map[string]string{"current_password": "test-password-123", "password": "new-password-456"}, nil), 200)
			case "logout":
				status(t, owner.request("POST", "/api/logout", nil, nil), 200)
			case "role":
				if _, err := a.changeUserRole(u.Username, "reviewer"); err != nil {
					t.Fatal(err)
				}
			case "operator reset":
				if err := a.provisionUser(u.Username, "operator-password-456", u.Role); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				if _, err := a.db.Exec("UPDATE sessions SET expires_at=0 WHERE user_id=?", u.ID); err != nil {
					t.Fatal(err)
				}
			case "deletion":
				status(t, admin.request("DELETE", fmt.Sprintf("/api/admin/users/%d", u.ID), nil, nil), 200)
				next := client(a, "192.0.2.3").register(t, "replacement")
				if next.ID <= u.ID {
					t.Fatal("deleted account ID reused")
				}
			}
			status(t, finish(), 401)
			var count int
			if err := a.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil || count != 0 {
				t.Fatal("revoked session created a token", count, err)
			}
		})
	}
}

func TestPendingWritesRejectRevokedCredentials(t *testing.T) {
	for _, operation := range []string{"create", "update", "upload", "crop", "settings", "vote", "approval"} {
		t.Run(operation, func(t *testing.T) {
			a := testApp(t)
			owner := client(a, "192.0.2.1")
			u := owner.provision(t, "owner", "reviewer")
			m := owner.addMod(t, "Original")
			w := owner.request("POST", modPath(m)+"/image", splitImage(t), nil)
			status(t, w, 200)
			m = readJSON[mod](t, w)
			writer := owner
			var tokenID int64
			if operation == "create" || operation == "update" || operation == "upload" || operation == "crop" {
				w = owner.request("POST", "/api/tokens", map[string]string{"name": "upload"}, nil)
				status(t, w, 201)
				token := readJSON[struct {
					ID    int64
					Token string
				}](t, w)
				tokenID = token.ID
				writer = client(a, "192.0.2.2")
				writer.token = token.Token
			}
			method, path, content := "POST", "/api/mods", "application/json"
			var input any = sampleMod("Changed")
			switch operation {
			case "update":
				method, path = "PUT", modPath(m)
				in := m.modInput
				in.Version = "2.0"
				input = in
			case "upload":
				path, content = modPath(m)+"/image", "image/png"
			case "crop":
				path, input = modPath(m)+"/image/crop", cropRect{0, 0, 100}
			case "settings":
				method, path, input = "PUT", "/api/settings", map[string]bool{"anonymous": true}
			case "vote":
				method, path, input = "PUT", modPath(m)+"/vote", map[string]int{"value": 1}
			case "approval":
				method, path, input = "PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "upload" {
				body = splitImage(t)
			}
			finish := pendingRequest(t, writer, method, path, content, body)
			if tokenID != 0 {
				status(t, owner.request("DELETE", fmt.Sprintf("/api/tokens/%d", tokenID), nil, nil), 200)
			} else if operation == "approval" {
				if _, err := a.changeUserRole(u.Username, "user"); err != nil {
					t.Fatal(err)
				}
			} else {
				status(t, owner.request("POST", "/api/logout", nil, nil), 200)
			}
			status(t, finish(), 401)
			current, err := a.readMod(m.ID, u.ID)
			if err != nil || !reflect.DeepEqual(m, current) {
				t.Fatal("revoked request changed mod", current, err)
			}
			var count int
			if err = a.db.QueryRow("SELECT COUNT(*) FROM mods").Scan(&count); err != nil || count != 1 {
				t.Fatal("revoked request created mod", count, err)
			}
			files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
			if err != nil || len(files) != 2 {
				t.Fatal("revoked image request changed stored files", files, err)
			}
		})
	}
}

func TestSessionIssuanceRejectsChangedAccount(t *testing.T) {
	for _, change := range []string{"password", "role", "deletion"} {
		t.Run(change, func(t *testing.T) {
			a := testApp(t)
			owner := client(a, "192.0.2.1")
			u := owner.register(t, "owner")
			var verifiedHash string
			if err := a.db.QueryRow("SELECT password FROM users WHERE id=?", u.ID).Scan(&verifiedHash); err != nil {
				t.Fatal(err)
			}
			// Account changes after login's password check but before session insertion.
			switch change {
			case "password":
				if err := a.provisionUser(u.Username, "new-password-456", u.Role); err != nil {
					t.Fatal(err)
				}
			case "role":
				if _, err := a.changeUserRole(u.Username, "reviewer"); err != nil {
					t.Fatal(err)
				}
			case "deletion":
				if _, err := a.db.Exec("DELETE FROM users WHERE id=?", u.ID); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			err := a.setSession(w, &u, verifiedHash)
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.status != http.StatusUnauthorized {
				t.Fatal("stale login issued session", err)
			}
			var count int
			if err = a.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil || count != 0 || len(w.Result().Cookies()) != 0 {
				t.Fatal("stale login left session or cookie", count, err)
			}
		})
	}
}
