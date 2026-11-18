package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func threadUpdatesPath(id int64) string {
	return fmt.Sprintf("/api/admin/mods/%d/thread-updates", id)
}

func followFixture(t *testing.T) (*app, *testClient, user, mod, importCandidate) {
	t.Helper()
	a, admin, owner, _ := galleryFixture(t)
	a.cfg.IngestOptions.owner = owner.ID
	modder := client(a, "192.0.2.2")
	modder.provision(t, "modder", "user")
	m := modder.addMod(t, "Unfplap")
	e := exampleExtraction()
	e.Name, e.Version = m.Name, "0.6.1"
	e.ShortDescription, e.Description = "Established overview.", "Manually maintained description."
	first := seedCandidate(t, a, 43720273, e)
	w := admin.request("POST", fmt.Sprintf("/api/admin/imports/%d", first.ID), map[string]any{"action": "publish", "target_id": m.ID, "revision": m.Revision}, nil)
	status(t, w, 200)
	m, err := a.readMod(m.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return a, admin, owner, m, first
}

func followSetting(t *testing.T, admin *testClient, id int64, enabled bool) threadUpdateState {
	t.Helper()
	path := threadUpdatesPath(id)
	w := admin.request("GET", path, nil, nil)
	status(t, w, 200)
	before := readJSON[threadUpdateState](t, w)
	w = admin.request("PUT", path, map[string]any{"enabled": enabled, "etag": before.ETag}, nil)
	status(t, w, 200)
	return readJSON[threadUpdateState](t, w)
}

func followRelease(first importCandidate) extractedMod {
	e := first.Entry
	e.Version, e.PreviousPostID = "0.6.2", first.PostID
	e.RepositoryURL = ""
	e.Downloads = []download{{Filename: "update.pck", URL: "https://u.pone.rs/new-release.pck"}}
	e.ShortDescription, e.Description = "Release-specific fix.", "These patch notes must not replace the overview."
	return e
}

func TestThreadUpdatesOptInPreservesOwnershipAndReleaseRules(t *testing.T) {
	a, admin, owner, m, first := followFixture(t)
	reviewer := client(a, "192.0.2.3")
	reviewer.provision(t, "reviewer", "reviewer")
	status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
	status(t, reviewer.request("PUT", modPath(m)+"/vote", map[string]int{"value": 1}, nil), 200)
	m, _ = a.readMod(m.ID, 0)
	e := followRelease(first)
	e.ImageURL = "https://example.org/first.png"
	c := seedCandidate(t, a, 43721895, e)
	if _, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err == nil || !strings.Contains(err.Error(), "Automatic updates are off") {
		t.Fatal("modder-owned listing did not require opt-in", err)
	}
	state := followSetting(t, admin, m.ID, true)
	if !state.Enabled || state.Paused || !state.HasSource {
		t.Fatal(state)
	}
	unchanged, _ := a.readMod(m.ID, 0)
	if !reflect.DeepEqual(m, unchanged) {
		t.Fatal("opt-in changed listing content or ownership", m, unchanged)
	}
	if err := a.publishPending(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := a.readMod(m.ID, 0)
	if after.UserID != m.UserID || after.Version != "0.6.2" || after.Revision != m.Revision+1 || after.Approval != "stale" || after.Rating != 1 || after.Description != m.Description || after.ShortDescription != m.ShortDescription || after.CreatedAt != m.CreatedAt || after.UpdatedAt != c.Source.Time || after.ImageURL == "" {
		t.Fatal("opt-in update lost data or skipped the release/image", after)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mod_images WHERE candidate_id=? AND mod_id=?", c.ID, m.ID).Scan(&count)
	if count != 1 {
		t.Fatal("source gallery missing", count)
	}
	if _, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err == nil {
		t.Fatal("replayed release")
	}
	// Following a modder's releases does not opt their description into synthesis.
	if err := a.summarizePending(context.Background(), &modelClient{}, owner.ID); err != nil {
		t.Fatal(err)
	}
	a.db.QueryRow("SELECT COUNT(*) FROM ingest_usage").Scan(&count)
	if count != 0 {
		t.Fatal("opt-in needed model calls", count)
	}
}

func TestThreadUpdatesKeepAmbiguityAndChronologyChecks(t *testing.T) {
	a, admin, owner, m, first := followFixture(t)
	followSetting(t, admin, m.ID, true)
	for n, tc := range []struct {
		name, want string
		change     func(*extractedMod)
		post       int64
	}{
		{"name-only", "ambiguous", func(e *extractedMod) { e.PreviousPostID = 0 }, 43721895},
		{"uncertain", "Extraction needs review", func(e *extractedMod) { e.Confidence = "uncertain" }, 43721896},
		{"downgrade", "downgrade", func(e *extractedMod) { e.Version = "0.5.9" }, 43721897},
		{"older post", "newer release post", func(e *extractedMod) {}, 43720000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := followRelease(first)
			tc.change(&e)
			c := seedCandidate(t, a, tc.post, e)
			if _, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(n, err)
			}
		})
	}
	after, _ := a.readMod(m.ID, 0)
	if !reflect.DeepEqual(m, after) {
		t.Fatal("held update changed listing", after)
	}
}

func TestThreadUpdatesDoNotClearManualEditHold(t *testing.T) {
	a, admin, owner, m, first := followFixture(t)
	m.Description = "Operator correction after the accepted release."
	status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
	state := followSetting(t, admin, m.ID, true)
	if !state.Paused || !strings.Contains(state.Reason, "manual edit") {
		t.Fatal(state)
	}
	c := seedCandidate(t, a, 43721895, followRelease(first))
	if _, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err == nil || !strings.Contains(err.Error(), "edited manually") {
		t.Fatal("opt-in bypassed manual edit", err)
	}
	followSetting(t, admin, m.ID, false)
	if !followSetting(t, admin, m.ID, true).Paused {
		t.Fatal("toggling resumed a manual edit")
	}
	status(t, admin.request("POST", fmt.Sprintf("/api/admin/imports/%d", c.ID), map[string]any{"action": "publish", "target_id": m.ID, "revision": m.Revision}, nil), 200)
	state = readJSON[threadUpdateState](t, admin.request("GET", threadUpdatesPath(m.ID), nil, nil))
	if state.Paused {
		t.Fatal("explicit release review did not resume following", state)
	}
	after, _ := a.readMod(m.ID, 0)
	if after.Description != m.Description || after.UserID != m.UserID {
		t.Fatal("review lost manual description or ownership", after)
	}
}

func TestThreadUpdateSettingsRequireAdminSessionAndCurrentState(t *testing.T) {
	a, admin, _, m, _ := followFixture(t)
	path := threadUpdatesPath(m.ID)
	before := readJSON[threadUpdateState](t, admin.request("GET", path, nil, nil))
	body := map[string]any{"enabled": true, "etag": before.ETag}
	for _, role := range []string{"user", "reviewer"} {
		c := client(a, "192.0.2.3")
		if role == "user" {
			c.login(t, "modder")
		} else {
			c.provision(t, "reviewer", role)
		}
		status(t, c.request("GET", path, nil, nil), 403)
		status(t, c.request("PUT", path, body, nil), 403)
	}
	guest := client(a, "192.0.2.4")
	status(t, guest.request("GET", path, nil, nil), 401)
	status(t, guest.request("PUT", path, body, nil), 401)
	token := readJSON[map[string]any](t, admin.request("POST", "/api/tokens", map[string]string{"name": "limited"}, nil))["token"].(string)
	guest.token = token
	status(t, guest.request("GET", path, nil, nil), 403)
	status(t, guest.request("PUT", path, body, nil), 403)
	status(t, admin.request("PUT", path, body, map[string]string{"X-CSRF-Token": "wrong"}), 403)
	status(t, admin.request("PUT", path, body, map[string]string{"Origin": "https://foreign.example"}), 403)
	status(t, admin.request("PUT", path, map[string]any{"enabled": true}, nil), 400)
	m.Description += " Modified."
	status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
	status(t, admin.request("PUT", path, body, nil), 409)
	state := followSetting(t, admin, m.ID, true)
	followSetting(t, admin, m.ID, false)
	status(t, admin.request("PUT", path, map[string]any{"enabled": true, "etag": state.ETag}, nil), 409)
	status(t, admin.request("GET", threadUpdatesPath(99999), nil, nil), 404)
	manual := admin.addMod(t, "No accepted release")
	state = readJSON[threadUpdateState](t, admin.request("GET", threadUpdatesPath(manual.ID), nil, nil))
	status(t, admin.request("PUT", threadUpdatesPath(manual.ID), map[string]any{"enabled": true, "etag": state.ETag}, nil), 409)
}

func TestThreadUpdatesCanDisableImporterOwnedListing(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	a.cfg.IngestOptions.owner = owner.ID
	e := exampleExtraction()
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := readJSON[threadUpdateState](t, admin.request("GET", threadUpdatesPath(id), nil, nil))
	if !state.Enabled || state.Paused {
		t.Fatal("default changed for importer-owned listings", state)
	}
	followSetting(t, admin, id, false)
	e.Version = "2.0"
	c = seedCandidate(t, a, 40000002, e)
	if _, err = a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err == nil || !strings.Contains(err.Error(), "Automatic updates are off") {
		t.Fatal(err)
	}
}

func TestThreadUpdateOptOutDuringImageFetch(t *testing.T) {
	a, admin, owner, m, first := followFixture(t)
	followSetting(t, admin, m.ID, true)
	e := followRelease(first)
	e.ImageURL = "https://example.org/first.png"
	c := seedCandidate(t, a, 43721895, e)
	if _, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	original := a.imageClient.Transport
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		followSetting(t, admin, m.ID, false)
		return original.RoundTrip(r)
	})
	if err := a.importCandidateImage(context.Background(), c, m.ID, owner.ID, nil); err == nil {
		t.Fatal("opt-out during fetch was ignored")
	}
	after, _ := a.readMod(m.ID, 0)
	files, err := filepath.Glob(filepath.Join(a.cfg.DataDir, "uploads", "*"))
	if err != nil || len(files) != 0 || after.ImageURL != "" {
		t.Fatal("revoked image or files persisted", files, err)
	}
}

func TestThreadUpdateMigrationPreservesDataAndCascades(t *testing.T) {
	a, admin, _, m, _ := followFixture(t)
	if _, err := a.db.Exec("DROP TABLE mod_thread_updates; PRAGMA user_version=9"); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	after, _ := a.readMod(m.ID, 0)
	if !reflect.DeepEqual(m, after) {
		t.Fatal("migration changed listing", after)
	}
	followSetting(t, admin, m.ID, true)
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("DELETE FROM users WHERE id=?", m.UserID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM mod_thread_updates").Scan(&count); err != nil || count != 0 {
		t.Fatal("owner deletion left a permission override", count, err)
	}
}
