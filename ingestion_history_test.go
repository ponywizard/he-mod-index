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

func historyRequest(c importCandidate, m mod) (string, map[string]any) {
	return fmt.Sprintf("/api/admin/imports/%d", c.ID), map[string]any{"action": "history", "target_id": m.ID, "revision": m.Revision}
}

func TestHistoryAcceptancePreservesReleaseAndManualContent(t *testing.T) {
	for _, kind := range []string{"imported", "edited import", "user listing"} {
		t.Run(kind, func(t *testing.T) {
			a, admin, owner, calls := galleryFixture(t)
			var m mod
			if kind == "user listing" {
				author := client(a, "192.0.2.2")
				author.register(t, "author")
				m = author.addMod(t, "Fishing")
			} else {
				_, m = publishImageFixture(t, a, owner, 40000020, "2.0", "current.png")
				if kind == "edited import" {
					m.Description = "Operator correction."
					status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
				}
			}
			reviewer := client(a, "192.0.2.3")
			reviewer.provision(t, "reviewer", "reviewer")
			status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
			status(t, reviewer.request("PUT", modPath(m)+"/vote", map[string]any{"value": 1}, nil), 200)
			before, _ := a.readMod(m.ID, 0)
			imageCalls := *calls
			e := exampleExtraction()
			e.ImageURL = "https://example.org/old.png"
			c := seedCandidate(t, a, 40000010, e)
			path, request := historyRequest(c, before)
			w := admin.request("POST", path, request, nil)
			status(t, w, 200)
			if readJSON[map[string]any](t, w)["warning"] != "" {
				t.Fatal(w.Body.String())
			}
			after, _ := a.readMod(m.ID, 0)
			if fingerprint(after) != fingerprint(before) || after.UserID != before.UserID || after.Author != before.Author || after.Revision != before.Revision || after.Approval != "current" || after.Rating != 1 || after.SourceURL != before.SourceURL {
				t.Fatal("history changed current release, manual content, ownership, votes or approval", before, after)
			}
			wantUpdated := before.UpdatedAt
			if kind == "user listing" {
				wantUpdated = c.Source.Time
			}
			if after.CreatedAt != c.Source.Time || after.UpdatedAt != wantUpdated {
				t.Fatal("history dates did not reflect accepted posts", after)
			}
			images := readJSON[[]modImage](t, admin.request("GET", modPath(m)+"/images", nil, nil))
			image := images[len(images)-1]
			if image.PostID != c.PostID || image.Version != "1.0" || image.SourceURL != "https://desuarchive.org/mlp/thread/40000000/#40000010" || *calls != imageCalls+1 {
				t.Fatal("history image lost provenance", images, *calls)
			}
			assertImagePair(t, a, image.ImageURL, true)
			status(t, admin.request("POST", path, request, nil), 409)
			if *calls != imageCalls+1 {
				t.Fatal("duplicate history fetched an image again")
			}
			if kind == "user listing" {
				var count int
				a.db.QueryRow("SELECT COUNT(*) FROM mod_sources WHERE mod_id=?", m.ID).Scan(&count)
				if count != 0 {
					t.Fatal("history granted automatic update provenance")
				}
				after.Description = "A further manual correction."
				w := admin.request("PUT", modPath(after), after.modInput, nil)
				status(t, w, 200)
				if edited := readJSON[mod](t, w); edited.UpdatedAt != wantUpdated || edited.Approval != "current" {
					t.Fatal("manual metadata edit changed source date or approval", edited)
				}
			} else {
				var post int64
				a.db.QueryRow("SELECT post_id FROM mod_sources WHERE mod_id=?", m.ID).Scan(&post)
				if post != 40000020 {
					t.Fatal("history moved current release pointer", post)
				}
			}
		})
	}
}

func TestHistoryAcceptanceAuthorizationAndConflicts(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	admin.provision(t, "operator", "admin")
	m := admin.addMod(t, "Fishing")
	c := seedCandidate(t, a, 40000010, exampleExtraction())
	path, request := historyRequest(c, m)
	status(t, client(a, "192.0.2.2").request("POST", path, request, nil), 401)
	ordinary := client(a, "192.0.2.3")
	ordinary.register(t, "ordinary")
	status(t, ordinary.request("POST", path, request, nil), 403)
	reviewer := client(a, "192.0.2.4")
	reviewer.provision(t, "reviewer", "reviewer")
	status(t, reviewer.request("POST", path, request, nil), 403)
	token := readJSON[map[string]any](t, admin.request("POST", "/api/tokens", map[string]any{"name": "limited"}, nil))["token"].(string)
	bearer := client(a, "192.0.2.5")
	bearer.token = token
	status(t, bearer.request("POST", path, request, nil), 403)
	status(t, admin.request("POST", path, request, map[string]string{"X-CSRF-Token": ""}), 403)
	request["revision"] = m.Revision + 1
	status(t, admin.request("POST", path, request, nil), 409)
	request["revision"] = m.Revision
	request["target_id"] = 0
	status(t, admin.request("POST", path, request, nil), 400)
	request["target_id"] = 99999
	status(t, admin.request("POST", path, request, nil), 404)
	var state string
	a.db.QueryRow("SELECT state FROM ingest_candidates WHERE id=?", c.ID).Scan(&state)
	if state != "pending" {
		t.Fatal("failed request changed candidate", state)
	}
	request["target_id"] = m.ID
	status(t, admin.request("POST", path, map[string]any{"action": "reject"}, nil), 200)
	status(t, admin.request("POST", path, request, nil), 409)
}

func TestHistoryRebuildsGeneratedOverviewButKeepsManualDescription(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	current := seedCandidate(t, a, 40000020, e)
	id, err := a.publishCandidate(context.Background(), current.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.readMod(id, 0)
	if _, err = a.db.Exec("INSERT INTO mod_overviews(mod_id,post_id) VALUES(?,?)", id, current.PostID); err != nil {
		t.Fatal(err)
	}
	old := seedCandidate(t, a, 40000010, e)
	path, request := historyRequest(old, m)
	status(t, admin.request("POST", path, request, nil), 200)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct{ Input string }
		json.NewDecoder(r.Body).Decode(&request)
		var input struct {
			Previous modOverview       `json:"previous_overview"`
			History  []modHistoryEntry `json:"history"`
		}
		json.Unmarshal([]byte(request.Input), &input)
		if input.Previous.Description != "" || len(input.History) != 2 || input.History[0].PostID != old.PostID || input.History[1].PostID != current.PostID {
			t.Error("older source was omitted from rebuild", request.Input)
		}
		fmt.Fprint(w, `{"status":"completed","usage":{"input_tokens":10,"output_tokens":10},"output":[{"content":[{"type":"output_text","text":"{\"short_description\":\"Overview\",\"description\":\"Complete history overview\"}"}]}]}`)
	}))
	defer server.Close()
	model := &modelClient{app: a, key: "test", model: "gpt-5.4-mini", budget: 1000000, client: server.Client(), endpoint: server.URL}
	if err = a.summarizePending(context.Background(), model, owner.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := a.readMod(id, 0)
	if got.Description != "Complete history overview" || calls != 1 || got.Revision != m.Revision {
		t.Fatal(got, calls)
	}
	got.Description = "Manual correction"
	status(t, admin.request("PUT", modPath(got), got.modInput, nil), 200)
	older := seedCandidate(t, a, 40000005, e)
	path, request = historyRequest(older, got)
	status(t, admin.request("POST", path, request, nil), 200)
	if err = a.summarizePending(context.Background(), model, owner.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = a.readMod(id, 0)
	if calls != 1 || got.Description != "Manual correction" {
		t.Fatal("history overwrote manual description", got, calls)
	}
}

func TestNewerHistoryDoesNotSelectCurrentRelease(t *testing.T) {
	a, admin, owner, _ := galleryFixture(t)
	_, m := publishImageFixture(t, a, owner, 40000020, "2.0", "current.png")
	e := exampleExtraction()
	c := seedCandidate(t, a, 40000030, e)
	path, request := historyRequest(c, m)
	status(t, admin.request("POST", path, request, nil), 200)
	e.Version = "2.5"
	next := seedCandidate(t, a, 40000025, e)
	if _, err := a.publishCandidate(context.Background(), next.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := a.readMod(m.ID, 0)
	if got.Version != "2.5" || got.UpdatedAt != c.Source.Time {
		t.Fatal("release update lost history dates", got)
	}
	e.ImageURL = "https://example.org/missing.png"
	missing := seedCandidate(t, a, 40000005, e)
	path, request = historyRequest(missing, got)
	w := admin.request("POST", path, request, nil)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), "History saved") {
		t.Fatal("image failure was hidden", w.Body.String())
	}
	history, err := readModHistory(context.Background(), a.db, m.ID)
	if err != nil || len(history) != 4 {
		t.Fatal("image failure lost accepted history", history, err)
	}
}
