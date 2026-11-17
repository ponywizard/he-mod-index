package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPruneRecommendationsAndRepeatedDownloads(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	first := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), first.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	repeat := seedCandidate(t, a, 40000002, e)
	quoted := seedCandidate(t, a, 40000003, e)
	quoted.Source.Text = ">>40000001\nTry that mod, it might help."
	raw, _ := json.Marshal(quoted.Source)
	if _, err = a.db.Exec("UPDATE ingest_posts SET payload=? WHERE id=?", string(raw), quoted.PostID); err != nil {
		t.Fatal(err)
	}
	e.Version = "2.0" // A new version may be released at a stable URL.
	newer := seedCandidate(t, a, 40000004, e)
	rejected := seedCandidate(t, a, 40000005, e)
	a.db.Exec("UPDATE ingest_candidates SET state='rejected',reason='Operator decision' WHERE id=?", rejected.ID)
	before, _ := a.readMod(id, 0)
	for _, dry := range []bool{true, false} {
		out, err := a.prunePendingImports(context.Background(), dry)
		if err != nil || len(out) != 2 || out[0].ID != repeat.ID || out[1].ID != quoted.ID {
			t.Fatal(out, err)
		}
		var state string
		a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", repeat.ID).Scan(&state)
		if dry && state != "pending" || !dry && state != "rejected" {
			t.Fatal(state, dry)
		}
	}
	for cid, want := range map[int64]string{first.ID: "published", newer.ID: "pending", rejected.ID: "rejected"} {
		var state string
		a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", cid).Scan(&state)
		if state != want {
			t.Fatal(cid, state)
		}
	}
	after, _ := a.readMod(id, 0)
	if fingerprint(before) != fingerprint(after) {
		t.Fatal("prune modified published content")
	}
	out, err := a.prunePendingImports(context.Background(), false)
	if err != nil || len(out) != 0 {
		t.Fatal(out, err)
	}
}

func TestSourceAttachmentAuthorizationAndCache(t *testing.T) {
	a, admin, owner, calls := galleryFixture(t)
	c := seedCandidate(t, a, 40000001, exampleExtraction())
	c.Source.Image = "https://example.org/post.png"
	raw, _ := json.Marshal(c.Source)
	if _, err := a.db.Exec("UPDATE ingest_posts SET payload=? WHERE id=?", string(raw), c.PostID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/source-posts/%d/image", c.PostID)
	guest := client(a, "192.0.2.2")
	status(t, guest.request("GET", path, nil, nil), 404)
	status(t, admin.request("GET", path, nil, nil), 200)
	status(t, admin.request("GET", path, nil, nil), 200)
	if *calls != 1 {
		t.Fatal("attachment was not cached", *calls)
	}
	status(t, guest.request("GET", path, nil, nil), 404) // Cache must not bypass authorization.
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	w := guest.request("GET", path, nil, nil)
	status(t, w, 200)
	if w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(w.Header())
	}
	m, _ := a.readMod(id, 0)
	if m.ImageURL != "" {
		t.Fatal("preview changed cover")
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mod_images").Scan(&count)
	if count != 0 {
		t.Fatal("preview changed gallery")
	}
	history := readJSON[[]modHistoryEntry](t, guest.request("GET", fmt.Sprintf("/api/mods/%d/history", id), nil, nil))
	if len(history) != 1 || history[0].Source.Text != c.Source.Text || history[0].Source.Image != c.Source.Image {
		t.Fatal(history)
	}
	a.db.Exec("DELETE FROM mods WHERE id=?", id)
	status(t, guest.request("GET", path, nil, nil), 404)
}

func TestMergeRedirectMigrationAndHistoricalAlias(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.db.Exec("UPDATE mods SET name='Better Fishing' WHERE id=?", id)
	e.RepositoryURL = ""
	e.Downloads = []download{{Filename: "new.pck", URL: "https://example.org/new.pck"}}
	next := seedCandidate(t, a, 40000002, e)
	if _, err = a.publishCandidate(context.Background(), next.ID, owner.ID, nil, nil, 0); err == nil {
		t.Fatal("historical name created a duplicate")
	}
	// Exercise the deployed v8 -> v9 migration, including repeat startup.
	if _, err = a.db.Exec("DROP TABLE mod_redirects; PRAGMA user_version=8"); err != nil {
		t.Fatal(err)
	}
	if err = migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO mod_redirects(old_id,mod_id) VALUES(99,?)", id); err != nil {
		t.Fatal(err)
	}
	if err = migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mod/99", "/api/mods/99/history?x=1"} {
		w := admin.request("GET", path, nil, nil)
		status(t, w, http.StatusPermanentRedirect)
		if w.Header().Get("Location") == "" {
			t.Fatal(w.Header())
		}
	}
	w := admin.request("PUT", "/api/mods/99", e.input(), nil)
	if w.Code == 308 {
		t.Fatal("write was redirected")
	}
	a.db.Exec("DELETE FROM users WHERE id=?", owner.ID)
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mod_redirects").Scan(&count)
	if count != 0 {
		t.Fatal("redirect survived owner cascade")
	}
}

// A release can include an attachment and discuss future work on a different mod.
// Neither should hide the actual current download or import a quoted older file.
func TestExtractorKeepsOwnReleaseAndAttachment(t *testing.T) {
	a := testApp(t)
	p := normalizePost(threadPost{ID: 40000002, Text: "Better Mods 1.2 is here: https://example.org/new.pck\nI am working on something else next.", Image: "https://example.org/menu.png"})
	old := normalizePost(threadPost{ID: 40000001, Text: "Old https://example.org/old.pck"})
	e := exampleExtraction()
	e.Name = "Better Mods"
	e.Version = "1.2"
	e.RepositoryURL = ""
	e.ImageURL = "" // The model may omit an attachment; source metadata still wins.
	e.Downloads = []download{{Filename: "new.pck", URL: p.Links[0]}, {Filename: "old.pck", URL: old.Links[0]}}
	payload, _ := json.Marshal(map[string]any{"entries": []extractedMod{e}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(payload)}}}}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 200}, "output": output})
	}))
	defer server.Close()
	model := &modelClient{app: a, key: "test", model: "gpt-5.4-mini", budget: 1000000, client: server.Client(), endpoint: server.URL}
	entries, err := model.extract(context.Background(), p, []threadPost{old})
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	got := entries[0]
	if got.ImageURL != p.Image || got.Confidence != "clear" || len(got.Downloads) != 1 || got.Downloads[0].URL != p.Links[0] {
		t.Fatal(got)
	}
}
