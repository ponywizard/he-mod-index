package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Two independently versioned packages announced together, as in post 43721459.
func multiModEntries() []extractedMod {
	return []extractedMod{
		{Name: "Queen Chrysalis", Version: "0.2.12", Downloads: []download{{Filename: "queen_chrysalis-0.2.12.zip", URL: "https://u.pone.rs/sozxsaeg.zip"}}, ShortDescription: "Adds Chrysalis with scheduled visits and disguises.", Description: "Chrysalis lives in the badlands and can visit the player.", Confidence: "clear"},
		{Name: "Move In With Player", Version: "0.1.4", Downloads: []download{{Filename: "move_in_with_player-0.1.4.zip", URL: "https://u.pone.rs/mtmljjyl.zip"}}, ShortDescription: "Invite ponies without a home to move into yours.", Confidence: "clear"},
	}
}
func extractMultiFixture(t *testing.T, a *app, postID int64, entries []extractedMod, classificationReason ...string) []importCandidate {
	t.Helper()
	p := threadPost{ID: postID, ThreadID: 43715264, Time: postID, Author: "Anonymous", URL: fmt.Sprintf("https://desuarchive.org/mlp/thread/43715264/#%d", postID)}
	for _, e := range entries {
		p.Text += e.Name + " " + e.Version + ": " + e.Downloads[0].URL + "\n" + e.ShortDescription + "\n"
		if e.PreviousPostID > 0 {
			p.Text += fmt.Sprintf(">>%d\n", e.PreviousPostID)
		}
	}
	p.Image = entries[0].ImageURL
	p = normalizePost(p)
	if err := a.savePosts(context.Background(), []threadPost{p}); err != nil {
		t.Fatal(err)
	}
	if len(classificationReason) > 0 {
		if _, err := a.db.Exec("UPDATE ingest_posts SET reason=? WHERE id=?", classificationReason[0], postID); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(map[string]any{"entries": entries})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(payload)}}}}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 200}, "output": output})
	}))
	defer server.Close()
	model := &modelClient{app: a, key: "test", model: "gpt-5.4-mini", budget: 1000000, client: server.Client(), endpoint: server.URL}
	if err := a.extractPost(context.Background(), model, p); err != nil {
		t.Fatal(err)
	}
	rows, err := a.db.Query(candidateSelect+" WHERE c.post_id=? ORDER BY c.ordinal", postID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []importCandidate
	for rows.Next() {
		c, err := candidateFrom(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(entries) {
		t.Fatal("lost sibling entry", out)
	}
	return out
}

func TestClassifierUncertaintyDoesNotOverridePerModExtraction(t *testing.T) {
	for _, screening := range []string{"uncertain: Update lacks full package details.", "release: Named update with downloads."} {
		t.Run(screening, func(t *testing.T) {
			a := testApp(t)
			admin := client(a, "192.0.2.1")
			owner := admin.provision(t, "operator", "admin")
			entries := multiModEntries()
			entries[0].Reason = "The target supplies a named, versioned release and its own download."
			entries[1].Confidence = "uncertain"
			entries[1].Reason = "The relationship between these packages is unclear."
			candidates := extractMultiFixture(t, a, 43721459, entries, screening)
			for i, c := range candidates {
				if c.Entry.Confidence != entries[i].Confidence || c.Entry.Reason != entries[i].Reason {
					t.Fatal("post screening replaced the per-mod extraction decision", c.Entry)
				}
			}
			if err := a.publishPending(context.Background(), owner.ID); err != nil {
				t.Fatal(err)
			}
			for i, want := range []string{"published", "pending"} {
				var state string
				if err := a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", candidates[i].ID).Scan(&state); err != nil || state != want {
					t.Fatal(i, state, err)
				}
			}
			var reason string
			if err := a.db.QueryRow("SELECT reason FROM ingest_posts WHERE id=43721459").Scan(&reason); err != nil || reason != screening {
				t.Fatal("lost screening evidence", reason, err)
			}
		})
	}
}

func TestContextResolvedUpdateAfterUncertainScreening(t *testing.T) {
	a, admin, owner, m, first := followFixture(t)
	followSetting(t, admin, m.ID, true)
	entries := []extractedMod{followRelease(first)}
	candidates := extractMultiFixture(t, a, 43721895, entries, "uncertain: Announces a versioned bugfix with a direct download, but no package details beyond changes.")
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	after, err := a.readMod(m.ID, 0)
	if err != nil || after.Version != "0.6.2" || after.UserID != m.UserID || after.Description != m.Description {
		t.Fatal("context-resolved update did not reach the original listing", after, err)
	}
	var state string
	if err = a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", candidates[0].ID).Scan(&state); err != nil || state != "published" {
		t.Fatal(state, err)
	}
}
func TestMultipleModsKeepIndependentReleasesAndHistory(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	original := multiModEntries()
	for i := range original {
		original[i].ImageURL = "https://i.4cdn.org/mlp/1794954327193823.webm"
	}
	first := extractMultiFixture(t, a, 43721459, original)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 2)
	for i, c := range first {
		if c.Entry.ImageURL != "" {
			t.Fatal("video was classified as a screenshot", c.Entry)
		}
		if err := a.db.QueryRow("SELECT mod_id FROM ingest_candidates WHERE id=?", c.ID).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
		m, err := a.readMod(ids[i], 0)
		if err != nil {
			t.Fatal(err)
		}
		if m.Name != original[i].Name || m.Version != original[i].Version || len(m.Downloads) != 1 || m.Downloads[0].URL != original[i].Downloads[0].URL {
			t.Fatal("mixed initial releases", m)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("combined distinct mods")
	}
	// Both updates quote the same original post. The individual names resolve them.
	next := multiModEntries()
	for i := range next {
		next[i].Version = "1.0"
		next[i].PreviousPostID = first[i].PostID
		next[i].Downloads = []download{{Filename: "updated.zip", URL: fmt.Sprintf("https://example.org/%d.zip", i)}}
	}
	updates := extractMultiFixture(t, a, 43721460, next)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	for i, c := range updates {
		var id int64
		var state string
		a.db.QueryRow("SELECT mod_id,state FROM ingest_candidates WHERE id=?", c.ID).Scan(&id, &state)
		if id != ids[i] || state != "published" {
			t.Fatal("shared source misidentified update", id, state)
		}
	}
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "reviewer", "reviewer")
	for _, id := range ids {
		m, _ := a.readMod(id, 0)
		status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
	}
	// A later update can quote older accepted history and still change only its mod.
	single := next[:1]
	single[0].Version = "1.1"
	single[0].Downloads = []download{{Filename: "new.zip", URL: "https://example.org/new.zip"}}
	extractMultiFixture(t, a, 43721461, single)
	beforeOther, _ := a.readMod(ids[1], 0)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	afterFirst, _ := a.readMod(ids[0], 0)
	afterOther, _ := a.readMod(ids[1], 0)
	if afterFirst.Version != "1.1" || afterFirst.Approval == "current" || afterOther.Approval != "current" || fingerprint(beforeOther) != fingerprint(afterOther) {
		t.Fatal("update crossed release boundaries", afterFirst, afterOther)
	}
	for i, id := range ids {
		history, err := readModHistory(context.Background(), a.db, id)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if i == 0 {
			want = 3
		}
		if len(history) != want {
			t.Fatal(history)
		}
		for _, h := range history {
			if h.Name != original[i].Name || len(h.Downloads) != 1 {
				t.Fatal("mixed history", h)
			}
		}
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mods").Scan(&count)
	if count != 2 {
		t.Fatal("duplicate listing", count)
	}
}
func TestMultiModDecisionsDoNotAffectSiblingCandidates(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	entries := multiModEntries()
	entries[1].Confidence = "uncertain"
	entries[1].Reason = "Confirm which package this is."
	candidates := extractMultiFixture(t, a, 43721459, entries)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	var firstID int64
	var state string
	a.db.QueryRow("SELECT mod_id FROM ingest_candidates WHERE id=?", candidates[0].ID).Scan(&firstID)
	a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", candidates[1].ID).Scan(&state)
	if firstID == 0 || state != "pending" {
		t.Fatal(firstID, state)
	}
	// A vague reference to the shared post cannot borrow the only published mod's identity.
	ambiguous := multiModEntries()[:1]
	ambiguous[0].Name = "Renamed Package"
	ambiguous[0].Version = "1.0"
	ambiguous[0].PreviousPostID = 43721459
	ambiguous[0].Downloads = []download{{Filename: "fix.zip", URL: "https://example.org/fix.zip"}}
	pending := extractMultiFixture(t, a, 43721460, ambiguous)
	if _, err := a.publishCandidate(context.Background(), pending[0].ID, owner.ID, nil, nil, 0); err == nil || !strings.Contains(err.Error(), "multiple mods") {
		t.Fatal("ambiguous shared post was assigned", err)
	}
	// Dismissing a sibling leaves the accepted listing and its immutable history intact.
	before, _ := a.readMod(firstID, 0)
	status(t, admin.request("POST", fmt.Sprintf("/api/admin/imports/%d", candidates[1].ID), map[string]any{"action": "reject"}, nil), 200)
	after, _ := a.readMod(firstID, 0)
	if fingerprint(before) != fingerprint(after) {
		t.Fatal("sibling dismissal changed listing")
	}
	var accepted string
	a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", candidates[0].ID).Scan(&accepted)
	if accepted != "published" {
		t.Fatal(accepted)
	}
}

func TestMultipleModsPruneOnlyRepeatedComponent(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	entries := multiModEntries()
	first := extractMultiFixture(t, a, 43721459, entries)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	entries[1].Version = "0.1.5"
	entries[1].Downloads = []download{{Filename: "move.zip", URL: "https://example.org/move.zip"}}
	for i := range entries {
		entries[i].PreviousPostID = 43721459
	}
	next := extractMultiFixture(t, a, 43721460, entries)
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"rejected", "published"} {
		var state string
		a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", next[i].ID).Scan(&state)
		if state != want {
			t.Fatal(i, state)
		}
	}
	var originalID, nextID int64
	a.db.QueryRow("SELECT mod_id FROM ingest_candidates WHERE id=?", first[1].ID).Scan(&originalID)
	a.db.QueryRow("SELECT mod_id FROM ingest_candidates WHERE id=?", next[1].ID).Scan(&nextID)
	if nextID != originalID {
		t.Fatal("new sibling did not update its own listing", originalID, nextID)
	}
}
