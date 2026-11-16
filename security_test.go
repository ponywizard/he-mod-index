package main

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	r.AddCookie(c.cookie)
	r.Header.Set("X-CSRF-Token", c.csrf)
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
