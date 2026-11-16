package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := newApp(db, config{PublicURL: "http://localhost:8080", DataDir: dir, RegistrationCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type testClient struct {
	a               *app
	cookie          *http.Cookie
	csrf, ip, token string
}

func client(a *app, ip string) *testClient { return &testClient{a: a, ip: ip} }
func (c *testClient) request(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	content := "application/json"
	if raw, ok := body.([]byte); ok {
		reader = bytes.NewReader(raw)
		content = "application/octet-stream"
	} else if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, reader)
	r.RemoteAddr = c.ip + ":12345"
	r.Header.Set("Content-Type", content)
	if c.cookie != nil {
		r.AddCookie(c.cookie)
		r.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.a.routes().ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "he_session" {
			c.cookie = cookie
			c.csrf = c.a.keyed("csrf:" + cookie.Value)
		}
	}
	return w
}
func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
	}
}
func readJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (c *testClient) register(t *testing.T, name string) user {
	t.Helper()
	w := c.request("POST", "/api/register", credentials{name, "test-password-123"}, nil)
	status(t, w, 201)
	return readJSON[user](t, w)
}
func (c *testClient) login(t *testing.T, name string) user {
	t.Helper()
	w := c.request("POST", "/api/login", credentials{name, "test-password-123"}, nil)
	status(t, w, 200)
	return readJSON[user](t, w)
}
func (c *testClient) provision(t *testing.T, name, role string) user {
	t.Helper()
	if err := c.a.provisionUser(name, "test-password-123", role); err != nil {
		t.Fatal(err)
	}
	return c.login(t, name)
}
func sampleMod(name string) modInput {
	return modInput{Name: name, Version: "1.0", GameVersion: "0.3.0-maredev", DownloadURL: "https://example.org/mod-1.pck", RepositoryURL: "https://github.com/example/mod", ShortDescription: "A test mod.", Description: "First line.\nSecond line."}
}
func (c *testClient) addMod(t *testing.T, name string) mod {
	t.Helper()
	w := c.request("POST", "/api/mods", sampleMod(name), nil)
	status(t, w, 201)
	return readJSON[mod](t, w)
}
func modPath(m mod) string { return fmt.Sprintf("/api/mods/%d", m.ID) }

func TestRegistrationAndSessions(t *testing.T) {
	a := testApp(t)
	c := client(a, "192.0.2.1")
	for _, name := range []string{"devanon", "DEVANON", "ponywizard", "admin"} {
		status(t, c.request("POST", "/api/register", credentials{name, "test-password-123"}, nil), 409)
	}
	u := c.register(t, "alice")
	if u.Role != "user" || c.cookie == nil || !c.cookie.HttpOnly || c.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid registration/session")
	}
	w := c.request("POST", "/api/register", credentials{"second", "test-password-123"}, nil)
	status(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry header")
	}
	other := client(a, "192.0.2.2")
	status(t, other.request("POST", "/api/register", credentials{"ALICE", "test-password-123"}, nil), 409)
	other.register(t, "bob")
	status(t, c.request("POST", "/api/mods", sampleMod("No CSRF"), map[string]string{"X-CSRF-Token": ""}), 403)
	status(t, c.request("POST", "/api/mods", sampleMod("Bad Origin"), map[string]string{"Origin": "https://evil.example"}), 403)
	status(t, c.request("POST", "/api/mods", sampleMod("Cross site"), map[string]string{"Sec-Fetch-Site": "cross-site"}), 403)
	status(t, c.request("POST", "/api/mods", sampleMod("No JSON"), map[string]string{"Content-Type": "text/plain"}), 415)
	w = c.request("GET", "/api/session", nil, nil)
	status(t, w, 200)
	var session struct {
		User user
		CSRF string
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	if session.User.ID != u.ID || session.CSRF != c.csrf {
		t.Fatal("session mismatch")
	}
	status(t, c.request("POST", "/api/logout", nil, nil), 200)
	status(t, c.request("POST", "/api/mods", sampleMod("Signed out"), nil), 401)
	c.login(t, "ALICE")
	bad := client(a, "192.0.2.3")
	for n := 0; n < 10; n++ {
		status(t, bad.request("POST", "/api/login", credentials{"missing", "wrong"}, nil), 401)
	}
	status(t, bad.request("POST", "/api/login", credentials{"missing", "wrong"}, nil), 429)
}

func TestPersistentCooldownAndConcurrentRegistration(t *testing.T) {
	a := testApp(t)
	c := client(a, "198.51.100.1")
	c.register(t, "first")
	// A new app instance retains both the hash key and cooldown.
	b, err := newApp(a.db, a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	status(t, client(b, "198.51.100.1").request("POST", "/api/register", credentials{"second", "test-password-123"}, nil), 429)
	if _, err = a.db.Exec("UPDATE limits SET expires_at=? WHERE key LIKE 'register:%'", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	client(b, "198.51.100.1").register(t, "second")
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"raceone", "racetwo"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			codes <- client(a, "198.51.100.2").request("POST", "/api/register", credentials{name, "test-password-123"}, nil).Code
		}(name)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[429] != 1 {
		t.Fatalf("concurrent registration: %v", counts)
	}
}

func TestVotesAndDeletionCascades(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.10")
	owner.register(t, "owner")
	m := owner.addMod(t, "First")
	voter := client(a, "192.0.2.11")
	v := voter.register(t, "voter")
	own := voter.addMod(t, "Voter's mod")
	second := client(a, "192.0.2.12")
	second.register(t, "second")
	third := client(a, "192.0.2.13")
	third.register(t, "third")
	put := func(c *testClient, value int) *httptest.ResponseRecorder {
		return c.request("PUT", modPath(m)+"/vote", map[string]int{"value": value}, nil)
	}
	status(t, put(client(a, "192.0.2.14"), 1), 401)
	status(t, put(voter, 1), 200)
	status(t, put(voter, 1), 200)
	got := readJSON[mod](t, put(voter, -1))
	if got.Rating != -1 || got.Downvotes != 1 {
		t.Fatalf("toggle duplicated vote: %+v", got)
	}
	second.ip = voter.ip
	status(t, put(second, 1), 409)
	second.ip = "192.0.2.12"
	status(t, put(second, 1), 200)
	voter.ip = second.ip
	status(t, put(voter, 1), 409)
	voter.ip = "192.0.2.11"
	status(t, put(third, 1), 200)
	status(t, put(third, 0), 200)
	status(t, put(third, 0), 200)
	status(t, voter.request("PUT", modPath(m)+"/vote", map[string]int{"value": 2}, nil), 400)
	status(t, voter.request("PUT", modPath(m)+"/vote", map[string]int{}, nil), 400)
	status(t, second.request("DELETE", modPath(own), nil, nil), 403)
	status(t, second.request("GET", "/api/admin/users", nil, nil), 403)
	admin := client(a, "192.0.2.20")
	adminUser := admin.provision(t, "ponywizard", "admin")
	status(t, admin.request("DELETE", fmt.Sprintf("/api/admin/users/%d", adminUser.ID), nil, nil), 400)
	status(t, admin.request("DELETE", fmt.Sprintf("/api/admin/users/%d", v.ID), nil, nil), 200)
	got = readJSON[mod](t, owner.request("GET", modPath(m), nil, nil))
	if got.Rating != 1 {
		t.Fatalf("deleted user's vote remains: %+v", got)
	}
	status(t, owner.request("GET", modPath(own), nil, nil), 404)
	status(t, voter.request("POST", "/api/mods", sampleMod("Gone"), nil), 401)
	status(t, admin.request("DELETE", modPath(m), nil, nil), 200)
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM votes").Scan(&count); err != nil || count != 0 {
		t.Fatal("mod votes were not removed", count, err)
	}
}

func TestApprovalReleaseLifecycle(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.register(t, "owner")
	m := owner.addMod(t, "Checked")
	reviewer := client(a, "192.0.2.2")
	reviewerUser := reviewer.provision(t, "devanon", "reviewer")
	approve := func(c *testClient, revision int64) *httptest.ResponseRecorder {
		return c.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": revision}, nil)
	}
	status(t, approve(owner, m.Revision), 403)
	w := approve(reviewer, m.Revision)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Approval != "current" || m.ApprovedBy != "devanon" {
		t.Fatal("missing current check")
	}
	in := m.modInput
	in.Description = "New description"
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Approval != "current" || m.Revision != 1 {
		t.Fatal("description invalidated check")
	}
	in.Version = "2.0"
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Approval != "stale" || m.Revision != 2 {
		t.Fatal("new version did not fade check")
	}
	status(t, approve(reviewer, 1), 409)
	status(t, approve(reviewer, 2), 200)
	in.Downloads[0].URL = "https://example.org/new.pck"
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Approval != "stale" || m.Revision != 3 {
		t.Fatal("download change did not fade check")
	}
	status(t, approve(reviewer, 3), 200)
	admin := client(a, "192.0.2.3")
	admin.provision(t, "ponywizard", "admin")
	status(t, approve(admin, 3), 403)
	status(t, admin.request("DELETE", fmt.Sprintf("/api/admin/users/%d", reviewerUser.ID), nil, nil), 200)
	m = readJSON[mod](t, owner.request("GET", modPath(m), nil, nil))
	if m.Approval != "none" {
		t.Fatal("removed reviewer check is still visible")
	}
}

func TestTokenScopeRevocationAndPasswordReset(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.provision(t, "ponywizard", "admin")
	other := client(a, "192.0.2.2")
	other.register(t, "other")
	foreign := other.addMod(t, "Foreign")
	w := owner.request("POST", "/api/tokens", map[string]string{"name": "Agent"}, nil)
	status(t, w, 201)
	var created struct {
		ID    int64
		Token string
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	agent := client(a, "192.0.2.1")
	agent.token = created.Token
	m := agent.addMod(t, "Automated")
	for _, test := range []struct {
		method, path string
		body         any
	}{{"GET", "/api/tokens", nil}, {"GET", "/api/session", nil}, {"POST", "/api/tokens", map[string]string{"name": "No"}}, {"GET", "/api/admin/users", nil}, {"PUT", modPath(m) + "/vote", map[string]int{"value": 1}}, {"PUT", modPath(m) + "/approval", map[string]any{"approved": true, "revision": 1}}, {"PUT", modPath(foreign), sampleMod("Stolen")}, {"DELETE", modPath(foreign), nil}} {
		status(t, agent.request(test.method, test.path, test.body, nil), 403)
	}
	status(t, agent.request("PUT", modPath(m), sampleMod("Updated"), nil), 200)
	status(t, owner.request("DELETE", fmt.Sprintf("/api/tokens/%d", created.ID), nil, nil), 200)
	status(t, agent.request("POST", "/api/mods", sampleMod("Revoked"), nil), 401)
	w = owner.request("POST", "/api/tokens", map[string]string{"name": "Second"}, nil)
	status(t, w, 201)
	json.Unmarshal(w.Body.Bytes(), &created)
	agent.token = created.Token
	status(t, owner.request("PUT", "/api/password", map[string]string{"current_password": "wrong", "password": "new-password-123"}, nil), 403)
	status(t, owner.request("PUT", "/api/password", map[string]string{"current_password": "test-password-123", "password": "new-password-123"}, nil), 200)
	status(t, agent.request("POST", "/api/mods", sampleMod("Revoked"), nil), 401)
	status(t, owner.request("POST", "/api/mods", sampleMod("Signed out"), nil), 401)
	status(t, owner.request("POST", "/api/login", credentials{"ponywizard", "new-password-123"}, nil), 200)
	if err := a.provisionUser("ponywizard", "test-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	status(t, owner.request("POST", "/api/mods", sampleMod("CLI reset"), nil), 401)
}

func TestImagesAndInputValidation(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.register(t, "owner")
	m := owner.addMod(t, "Images")
	for _, url := range []string{"javascript:alert(1)", "data:text/plain,no", "https://user:password@example.org/file", "//example.org/x"} {
		in := sampleMod("Bad")
		in.DownloadURL = url
		status(t, owner.request("POST", "/api/mods", in, nil), 400)
	}
	var buffer bytes.Buffer
	png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	w := owner.request("POST", modPath(m)+"/image", buffer.Bytes(), nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if !validLocalImage(m.ImageURL, m.ID) {
		t.Fatalf("invalid generated image URL: %s", m.ImageURL)
	}
	w = owner.request("GET", m.ImageURL, nil, nil)
	status(t, w, 200)
	if w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("image content type")
	}
	status(t, owner.request("PUT", modPath(m), m.modInput, nil), 200)
	status(t, owner.request("POST", modPath(m)+"/image", []byte("<svg></svg>"), nil), 400)
	status(t, owner.request("POST", modPath(m)+"/image", make([]byte, 4<<20+1), nil), 413)
	second := owner.addMod(t, "Other")
	in := second.modInput
	in.ImageURL = m.ImageURL
	status(t, owner.request("PUT", modPath(second), in, nil), 400)
	status(t, owner.request("DELETE", modPath(m), nil, nil), 200)
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, strings.TrimPrefix(m.ImageURL, "/"))); !os.IsNotExist(err) {
		t.Fatal("deleted mod image remains", err)
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/mod/1", "/AGENTS.md", "/healthz"} {
		status(t, owner.request("GET", path, nil, nil), 200)
	}
	status(t, owner.request("GET", "/go.mod", nil, nil), 404)
	status(t, owner.request("GET", "/uploads/no.jpg", nil, nil), 404)
	w = owner.request("GET", "/default-mod.png", nil, nil)
	status(t, w, 200)
	if w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("default icon content type")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil || cfg.Width != 256 || cfg.Height != 256 {
		t.Fatal("bundled default icon could not be decoded", cfg, err)
	}
}

func TestProxyTrustAndIPv6(t *testing.T) {
	a := testApp(t)
	tests := []struct {
		remote, forwarded, want string
		trusted                 []netip.Prefix
	}{
		{"192.0.2.1:1234", "198.51.100.1", "192.0.2.1", nil},
		{"127.0.0.1:1234", "198.51.100.1", "127.0.0.1", nil},
		{"127.0.0.1:1234", "198.51.100.1", "198.51.100.1", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}},
		{"127.0.0.1:1234", "203.0.113.1, 198.51.100.1", "198.51.100.1", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}},
		{"[2001:db8:1234:5678::1]:1234", "", "2001:db8:1234:5678::/64", nil},
		{"[::ffff:192.0.2.1]:1234", "", "192.0.2.1", nil},
	}
	for _, test := range tests {
		a.cfg.TrustedProxies = test.trusted
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = test.remote
		r.Header.Set("X-Forwarded-For", test.forwarded)
		if got := a.clientIP(r); got != test.want {
			t.Errorf("clientIP(%s,%s)=%s want %s", test.remote, test.forwarded, got, test.want)
		}
	}
}
