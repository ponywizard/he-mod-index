package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedCandidate(t *testing.T, a *app, postID int64, e extractedMod) importCandidate {
	t.Helper()
	p := normalizePost(threadPost{ID: postID, ThreadID: 40000000, Time: postID, Author: "Anonymous", Text: "Released " + e.Name, URL: "https://desuarchive.org/mlp/thread/40000000/#post"})
	for _, d := range e.Downloads {
		p.Links = append(p.Links, d.URL)
	}
	if e.RepositoryURL != "" {
		p.Links = append(p.Links, e.RepositoryURL)
	}
	if err := a.savePosts(context.Background(), []threadPost{p}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	result, err := a.db.Exec("INSERT INTO ingest_candidates(post_id,ordinal,payload) VALUES(?,0,?)", postID, string(b))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return importCandidate{ID: id, PostID: postID, Entry: e, Source: p}
}
func exampleExtraction() extractedMod {
	return extractedMod{Name: "Fishing", Version: "1.0", Downloads: []download{{Filename: "fishing.pck", URL: "https://github.com/example/mods/releases/download/v1/fishing.pck"}}, RepositoryURL: "https://github.com/example/mods", ShortDescription: "Adds fishing.", Confidence: "clear"}
}
func TestIngestionPublicationPreservesReviewAndOwnershipRules(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	u := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.readMod(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Author != "Anonymous" || m.UserID != u.ID || m.SourceURL != c.Source.URL || m.Approval != "none" {
		t.Fatal(m)
	}
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err == nil {
		t.Fatal("replayed candidate")
	}
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "reviewer", "reviewer")
	status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
	e.Description = "More detail"
	c = seedCandidate(t, a, 40000002, e)
	next, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil || next != id {
		t.Fatal(next, err)
	}
	m, _ = a.readMod(id, 0)
	if m.Approval != "current" || m.Revision != 1 {
		t.Fatal("description invalidated review", m)
	}
	e.Version = "2.0"
	e.Downloads[0].URL = "https://github.com/example/mods/releases/download/v2/fishing.pck"
	c = seedCandidate(t, a, 40000003, e)
	next, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil || next != id {
		t.Fatal(next, err)
	}
	m, _ = a.readMod(id, 0)
	if m.Approval != "stale" || m.Revision != 2 {
		t.Fatal("release kept current check", m)
	}
	m.Description = "Operator correction"
	status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
	c = seedCandidate(t, a, 40000004, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err == nil {
		t.Fatal("overwrote manual correction")
	}
	// Ordinary sessions and bearer tokens cannot resolve the queue.
	ordinary := client(a, "192.0.2.3")
	ordinary.register(t, "ordinary")
	status(t, ordinary.request("POST", "/api/admin/imports/1", map[string]any{"action": "reject"}, nil), 403)
	token := admin.request("POST", "/api/tokens", map[string]string{"name": "limited"}, nil)
	status(t, token, 201)
	apiToken := readJSON[map[string]any](t, token)["token"].(string)
	bearer := client(a, "192.0.2.4")
	bearer.token = apiToken
	status(t, bearer.request("POST", "/api/admin/imports/1", map[string]any{"action": "reject"}, nil), 403)
	// User deletion removes mods and provenance, while old published candidates
	// remain handled and cannot resurrect the listing.
	if _, err = a.db.Exec("DELETE FROM users WHERE id=?", u.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mod_sources").Scan(&count)
	if count != 0 {
		t.Fatal("provenance did not cascade")
	}
	a.db.QueryRow("SELECT COUNT(*) FROM ingest_candidates WHERE state='published' AND mod_id IS NOT NULL").Scan(&count)
	if count != 0 {
		t.Fatal("dangling mod references")
	}
}
func TestIngestionHoldsAmbiguousIdentityAndOldSources(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	u := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	e.RepositoryURL = ""
	e.Downloads[0].URL = "https://example.org/file1.pck"
	c := seedCandidate(t, a, 40000010, e)
	id, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.Downloads[0].URL = "https://example.org/file2.pck"
	c = seedCandidate(t, a, 40000011, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err == nil {
		t.Fatal("matched solely by name")
	}
	e.PreviousPostID = 40000010
	c = seedCandidate(t, a, 40000012, e)
	if got, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err != nil || got != id {
		t.Fatal(got, err)
	}
	c = seedCandidate(t, a, 40000009, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, &id, 2); err == nil {
		t.Fatal("rolled back to older source")
	}
	e.Name = "Uncertain"
	e.Confidence = "uncertain"
	c = seedCandidate(t, a, 40000013, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err == nil {
		t.Fatal("published uncertain extraction")
	}
}
func TestSourceParsingAndCrossSourceDeduplication(t *testing.T) {
	b := []byte(`{"posts":[{"no":40000000,"time":100,"sub":"Home: Equestria #1","com":"OP"},{"no":40000001,"resto":40000000,"time":101,"com":"&gt;&gt;40000000<br>New mod https://example.org/relea<wbr>se.pck?a=1&amp;b=2","tim":123,"ext":".png"}]}`)
	posts, err := parseLiveThread(b)
	if err != nil {
		t.Fatal(err)
	}
	p := posts[1]
	if len(p.Links) != 1 || p.Links[0] != "https://example.org/release.pck?a=1&b=2" || len(p.Replies) != 1 || p.Replies[0] != 40000000 || p.Image != "https://i.4cdn.org/mlp/123.png" {
		t.Fatal(p)
	}
	archive := []byte(`{"40000000":{"op":{"num":"40000000","thread_num":"40000000","subnum":"0","timestamp":100},"posts":{"1":{"num":"40000001","thread_num":"40000000","subnum":"0","timestamp":101,"comment":"Release"},"2":{"num":"40000001","thread_num":"40000000","subnum":"1","timestamp":101,"comment":"Ghost"}}}}`)
	old, err := parseArchiveThread(archive, 40000000)
	if err != nil || len(old) != 2 {
		t.Fatal(old, err)
	}
	a := testApp(t)
	if err = a.savePosts(context.Background(), posts); err != nil {
		t.Fatal(err)
	}
	if err = a.savePosts(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM ingest_posts").Scan(&count)
	if count != 2 {
		t.Fatal("cross-source duplicate", count)
	}
	catalog := []byte(`[{"threads":[{"no":40000002,"sub":"Unrelated"},{"no":40000003,"sub":"Home: Equestria #2"}]}]`)
	next, err := currentThreadFromCatalog(catalog, 40000000)
	if err != nil || next != 40000003 {
		t.Fatal(next, err)
	}
	next, err = currentThreadFromCatalog(catalog, 40000004)
	if err != nil || next != 40000004 {
		t.Fatal("thread rolled backward", next, err)
	}
}
func TestExtractionRejectsInventedLinksAndPredecessors(t *testing.T) {
	e := exampleExtraction()
	p := threadPost{ID: 40000001, Links: []string{e.Downloads[0].URL, e.RepositoryURL}}
	if err := validateExtraction(e, p, nil); err != nil {
		t.Fatal(err)
	}
	e.Downloads[0].URL = "https://invented.example/mod.pck"
	if err := validateExtraction(e, p, nil); err == nil {
		t.Fatal("invented link accepted")
	}
	e = exampleExtraction()
	e.PreviousPostID = 999
	if err := validateExtraction(e, p, nil); err == nil {
		t.Fatal("invented predecessor accepted")
	}
	e = exampleExtraction()
	e.ImageURL = "http://localhost/private.png"
	if err := validateExtraction(e, p, nil); err == nil {
		t.Fatal("invented image accepted")
	}
}
func TestModelBudgetSurvivesErrorsAndRestart(t *testing.T) {
	a := testApp(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["store"] != false || body["tools"] != nil || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("invalid API request")
		}
		http.Error(w, "unavailable", 503)
	}))
	defer server.Close()
	c := &modelClient{app: a, key: "test-key", model: "gpt-6-luna", budget: 12000, client: server.Client(), endpoint: server.URL}
	for attempts := 0; attempts < 5; attempts++ {
		var out any
		_ = c.call(context.Background(), "test", "instructions", "data", schemaObject(map[string]any{}), &out)
	}
	if calls < 1 || calls > 3 {
		t.Fatal("budget did not stop failed calls", calls)
	}
	var spent int64
	a.db.QueryRow("SELECT SUM(reserved_micros) FROM ingest_usage").Scan(&spent)
	restarted := *c
	restarted.budget = spent
	var out any
	if err := restarted.call(context.Background(), "test", "instructions", "data", schemaObject(map[string]any{}), &out); err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatal(err)
	}
}
func TestPollingConfigAndIngestionLock(t *testing.T) {
	t.Setenv("INGEST_POLL_INTERVAL", "10s")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted rapid polling")
	}
	t.Setenv("INGEST_POLL_INTERVAL", "10m")
	t.Setenv("INGEST_OWNER_ID", "2")
	c, err := loadConfig()
	if err != nil || c.IngestInterval != 10*time.Minute || c.IngestOptions.owner != 2 {
		t.Fatal(c, err)
	}
	a := testApp(t)
	release, err := a.ingestionLock()
	if err != nil {
		t.Fatal(err)
	}
	if release2, err := a.ingestionLock(); err == nil {
		release2()
		t.Fatal("concurrent importer allowed")
	}
	release()
	release, err = a.ingestionLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestPollingCollectsPredecessorsAndPersistsConditionalRequests(t *testing.T) {
	a := testApp(t)
	_, err := a.db.Exec("INSERT INTO settings(key,value) VALUES('thread_watch:43710000','active')")
	if err != nil {
		t.Fatal(err)
	}
	f := &threadFetcher{}
	requests := []string{}
	f.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.last = time.Now().Add(-2 * time.Second)
		requests = append(requests, r.URL.Path)
		status := 200
		body := ""
		header := make(http.Header)
		switch {
		case r.URL.Path == "/mlp/catalog.json":
			body = `[{"threads":[{"no":43730000,"sub":"Home: Equestria #12"}]}]`
		case strings.Contains(r.URL.Path, "/search/"):
			body = `{"0":{"posts":[{"num":"43720000","thread_num":"43720000"}]},"meta":{"total_found":1}}`
		case strings.Contains(r.URL.Path, "/api/chan/thread/"):
			id := r.URL.Query().Get("num")
			body = `{"` + id + `":{"op":{"num":"` + id + `","thread_num":"` + id + `","timestamp":100},"posts":{}}}`
		case r.URL.Path == "/mlp/thread/43730000.json":
			if r.Header.Get("If-Modified-Since") == "Tue, 17 Nov 2026 10:00:00 GMT" {
				status = 304
			} else {
				body = `{"posts":[{"no":43730000,"time":200,"sub":"Home: Equestria #12"}]}`
				header.Set("Last-Modified", "Tue, 17 Nov 2026 10:00:00 GMT")
			}
		default:
			status = 404
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err = a.pollThreadsWith(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	var current string
	a.db.QueryRow("SELECT value FROM settings WHERE key='current_thread'").Scan(&current)
	if current != "43730000" {
		t.Fatal(current)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM ingest_posts").Scan(&count)
	if count != 4 {
		t.Fatal("missed predecessor/intermediate thread", count, requests)
	}
	var watch string
	a.db.QueryRow("SELECT value FROM settings WHERE key='thread_watch:43710000'").Scan(&watch)
	if watch != "closed" {
		t.Fatal("did not close archived predecessor", watch)
	}
	requests = nil
	if err = a.pollThreadsWith(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatal("refetched closed threads", requests)
	}
	a.db.QueryRow("SELECT COUNT(*) FROM ingest_posts").Scan(&count)
	if count != 4 {
		t.Fatal("304 duplicated posts")
	}
}

func TestImageImportRevalidatesOwnerAfterFetch(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	u := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Entry.ImageURL = "https://example.org/image.png"
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	if err = png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	a.imageClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, err := a.db.Exec("UPDATE users SET role='user' WHERE id=?", u.ID); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b.Bytes())), Header: make(http.Header)}, nil
	})}
	if err = a.importCandidateImage(context.Background(), c, id, u.ID, nil); err == nil {
		t.Fatal("revoked importer saved image")
	}
	m, _ := a.readMod(id, 0)
	if m.ImageURL != "" {
		t.Fatal("revoked image persisted")
	}
	paths, _ := filepath.Glob(filepath.Join(a.cfg.DataDir, "uploads", "*"))
	if len(paths) != 0 {
		t.Fatal("orphan image files", paths)
	}
}

func TestStagingMergeIsAdditiveAndIdempotent(t *testing.T) {
	source := testApp(t)
	e := exampleExtraction()
	candidate := seedCandidate(t, source, 40000001, e)
	if _, err := source.db.Exec("UPDATE ingest_posts SET state='done'; INSERT INTO ingest_usage(model,stage,reserved_micros,actual_micros,created_at) VALUES('gpt-6-luna','extract',1000,500,100)"); err != nil {
		t.Fatal(err)
	}
	// Version 6 staging snapshots remain readable after the history migration.
	if _, err := source.db.Exec("DROP TABLE mod_overviews; PRAGMA user_version=6"); err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	c := client(target, "192.0.2.1")
	c.register(t, "existing")
	m := c.addMod(t, "Existing mod")
	for i := 0; i < 2; i++ {
		if err := target.mergeIngestion(context.Background(), filepath.Join(source.cfg.DataDir, "index.db")); err != nil {
			t.Fatal(err)
		}
	}
	var count, spent int
	target.db.QueryRow("SELECT COUNT(*) FROM ingest_candidates").Scan(&count)
	if count != 1 {
		t.Fatal("duplicated candidate", count)
	}
	target.db.QueryRow("SELECT SUM(actual_micros) FROM ingest_usage").Scan(&spent)
	if spent != 500 {
		t.Fatal("duplicated API spending", spent)
	}
	got, err := target.readMod(m.ID, 0)
	if err != nil || got.Name != m.Name || got.UserID != m.UserID {
		t.Fatal("merge replaced existing listing", got, err)
	}
	target.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	if count != 1 {
		t.Fatal("copied account data", count)
	}
	imported, err := candidateFrom(target.db.QueryRow(candidateSelect+" WHERE c.post_id=?", candidate.PostID))
	if err != nil || imported.Entry.Name != e.Name {
		t.Fatal(imported, err)
	}
}

func TestExtractorHoldsQuotesAndDiscardsUnrelatedContext(t *testing.T) {
	a := testApp(t)
	p := normalizePost(threadPost{ID: 40000001, Text: ">https://example.org/mod.pck\nTried it and it crashes. Any fix?", Image: "https://example.org/reaction.gif"})
	contextPost := normalizePost(threadPost{ID: 40000000, Text: "New other mod https://example.org/other.pck", Image: "https://example.org/old.png"})
	quoted := exampleExtraction()
	quoted.Downloads = []download{{Filename: "bad/filename", URL: p.Links[0]}}
	quoted.RepositoryURL = ""
	quoted.GameVersion = "Home: Equestria"
	quoted.ImageURL = contextPost.Image
	unrelated := exampleExtraction()
	unrelated.Name = "Other"
	unrelated.Downloads = []download{{Filename: "other.pck", URL: contextPost.Links[0]}}
	unrelated.RepositoryURL = ""
	base := exampleExtraction()
	base.Name = "Home: Equestria"
	contents, _ := json.Marshal(map[string]any{"entries": []extractedMod{quoted, unrelated, base}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(contents)}}}}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 200}, "output": output})
	}))
	defer server.Close()
	model := &modelClient{app: a, key: "test", model: "gpt-5.4-mini", budget: 1000000, client: server.Client(), endpoint: server.URL}
	entries, err := model.extract(context.Background(), p, []threadPost{contextPost})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("kept base game or context-only release", entries)
	}
	e := entries[0]
	if e.Confidence != "uncertain" || e.GameVersion != "" || e.ImageURL != "" || e.Version != "Unversioned" || e.Downloads[0].Filename != "mod.pck" {
		t.Fatal("unguarded model fields", e)
	}
}

func TestBadStagingMergeRollsBackAllRows(t *testing.T) {
	source := testApp(t)
	e := exampleExtraction()
	c := seedCandidate(t, source, 40000001, e)
	e.Downloads[0].URL = "https://invented.example/new.pck"
	b, _ := json.Marshal(e)
	if _, err := source.db.Exec("UPDATE ingest_candidates SET payload=? WHERE id=?", string(b), c.ID); err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	if err := target.mergeIngestion(context.Background(), filepath.Join(source.cfg.DataDir, "index.db")); err == nil {
		t.Fatal("accepted fabricated URL")
	}
	var count int
	target.db.QueryRow("SELECT COUNT(*) FROM ingest_posts").Scan(&count)
	if count != 0 {
		t.Fatal("partial staging merge", count)
	}
}

func TestImportNameAliasesAndVersionDowngrades(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	u := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	e.Name = "Fishing-Mod"
	e.Version = "2.1"
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.Name = "Fishing"
	e.Version = "Unversioned"
	c = seedCandidate(t, a, 40000002, e)
	next, err := a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil || next != id {
		t.Fatal("name alias duplicated", next, err)
	}
	m, _ := a.readMod(id, 0)
	if m.Version != "2.1" || m.Revision != 1 {
		t.Fatal("unknown version erased known release", m)
	}
	e.Version = "1.9"
	c = seedCandidate(t, a, 40000003, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0); err == nil {
		t.Fatal("automatically downgraded version")
	}
	e.Name = "Renamed Fishing"
	e.Version = "2.2"
	e.PreviousPostID = 40000002
	c = seedCandidate(t, a, 40000004, e)
	next, err = a.publishCandidate(context.Background(), c.ID, u.ID, nil, nil, 0)
	if err != nil || next != id {
		t.Fatal("explicit predecessor did not match rename", next, err)
	}
}
