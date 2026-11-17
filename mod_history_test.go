package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOverviewUsesPublishedHistoryAndKeepsSourceDates(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "private_operator", "admin")
	e := exampleExtraction()
	e.Description = "Fish in ponds using a fishing rod. Install in the mods directory."
	first := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), first.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.ShortDescription = "Fixes a fishing animation."
	e.Description = "Fixed the rod animation."
	second := seedCandidate(t, a, 40000002, e)
	if _, err = a.publishCandidate(context.Background(), second.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := a.readMod(id, 0)
	if before.Description != "Fish in ponds using a fishing rod. Install in the mods directory." || before.CreatedAt != first.Source.Time || before.UpdatedAt != second.Source.Time {
		t.Fatal("update replaced overview or used import dates", before)
	}
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "reviewer", "reviewer")
	status(t, reviewer.request("PUT", modPath(before)+"/approval", map[string]any{"approved": true, "revision": before.Revision}, nil), 200)
	// A held candidate, even if associated with this mod, is not public history.
	held := seedCandidate(t, a, 40000003, e)
	if _, err = a.db.Exec("UPDATE ingest_candidates SET mod_id=? WHERE id=?", id, held.ID); err != nil {
		t.Fatal(err)
	}
	anonymous := client(a, "192.0.2.3")
	w := anonymous.request("GET", modPath(before)+"/history", nil, nil)
	status(t, w, 200)
	history := readJSON[[]modHistoryEntry](t, w)
	if len(history) != 2 || history[0].PostID != second.PostID || history[1].PostID != first.PostID || history[1].Description != before.Description || strings.Contains(w.Body.String(), "sourceText") || strings.Contains(w.Body.String(), "private_operator") {
		t.Fatal(w.Body.String())
	}
	status(t, anonymous.request("GET", "/api/mods/999/history", nil, nil), 404)

	calls := 0
	overall := modOverview{"Adds fishing with a rod.", "Fish in ponds using a rod. Install in the mods directory. The rod animation is fixed."}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var input struct {
			Previous modOverview       `json:"previous_overview"`
			History  []modHistoryEntry `json:"history"`
		}
		if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
			t.Error(err)
		}
		if calls == 1 {
			if len(input.History) != 2 || input.History[0].PostID != first.PostID || input.History[1].PostID != second.PostID || input.Previous.Description != "" {
				t.Error("initial synthesis lost history", request.Input)
			}
		} else if len(input.History) != 1 || input.History[0].PostID != held.PostID || input.Previous.Description != overall.Description {
			t.Error("incremental synthesis lost overview", request.Input)
		}
		contents, _ := json.Marshal(overall)
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(contents)}}}}})
	}))
	defer server.Close()
	model := &modelClient{app: a, key: "test", model: "gpt-5.4-mini", budget: 1000000, client: server.Client(), endpoint: server.URL}
	if err = a.summarizeMod(context.Background(), model, id, owner.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := a.readMod(id, 0)
	if got.Description != overall.Description || got.ShortDescription != overall.ShortDescription || got.Revision != before.Revision || got.Approval != "current" || got.CreatedAt != first.Source.Time || got.UpdatedAt != second.Source.Time {
		t.Fatal("summary changed release metadata", got)
	}
	if err = a.summarizePending(context.Background(), model, owner.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("repeat summary spent again", calls)
	}
	if _, err = a.publishCandidate(context.Background(), held.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal("summary blocked subsequent imports", err)
	}
	if err = a.summarizeMod(context.Background(), model, id, owner.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	next := seedCandidate(t, a, 40000004, e)
	if _, err = a.publishCandidate(context.Background(), next.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = a.readMod(id, 0)
	got.Description = "Operator correction."
	status(t, admin.request("PUT", modPath(got), got.modInput, nil), 200)
	if err = a.summarizeMod(context.Background(), model, id, owner.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = a.readMod(id, 0)
	if calls != 2 || got.Description != "Operator correction." || got.UpdatedAt != next.Source.Time {
		t.Fatal("summary overwrote manual correction or changed source date", calls, got)
	}
	if _, err = a.db.Exec("DELETE FROM users WHERE id=?", owner.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM mod_overviews").Scan(&count); err != nil || count != 0 {
		t.Fatal("overview progress did not cascade", count, err)
	}
}

func TestOverviewRejectsConcurrentChanges(t *testing.T) {
	for _, change := range []string{"manual edit", "new source", "owner role", "older history"} {
		t.Run(change, func(t *testing.T) {
			a := testApp(t)
			owner := client(a, "192.0.2.1").provision(t, "operator", "admin")
			e := exampleExtraction()
			c := seedCandidate(t, a, 40000001, e)
			id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := a.readMod(id, 0)
			history, err := readModHistory(context.Background(), a.db, id)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "manual edit":
				_, err = a.db.Exec("UPDATE mods SET description='Manual correction' WHERE id=?", id)
			case "new source":
				next := seedCandidate(t, a, 40000002, e)
				_, err = a.publishCandidate(context.Background(), next.ID, owner.ID, nil, nil, 0)
			case "owner role":
				_, err = a.db.Exec("UPDATE users SET role='user' WHERE id=?", owner.ID)
			case "older history":
				old := seedCandidate(t, a, 39999999, e)
				_, err = a.db.Exec("UPDATE ingest_candidates SET state='published',mod_id=? WHERE id=?", id, old.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = a.saveOverview(context.Background(), before, c.PostID, historyFingerprint(history), modOverview{"Overview", "Should not save"}); err == nil {
				t.Fatal("saved stale model output")
			}
			got, _ := a.readMod(id, 0)
			if got.Description == "Should not save" {
				t.Fatal(got)
			}
		})
	}
}

func TestVersionSevenBackfillsOnlyImportedDates(t *testing.T) {
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "operator", "admin")
	e := exampleExtraction()
	first := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), first.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	second := seedCandidate(t, a, 40000002, e)
	if _, err = a.publishCandidate(context.Background(), second.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := a.readMod(id, 0)
	normal := e.input()
	normal.Name = "Manual listing"
	response := admin.request("POST", "/api/mods", normal, nil)
	status(t, response, 201)
	manual := readJSON[mod](t, response)
	if _, err = a.db.Exec("UPDATE mods SET created_at=100,updated_at=200 WHERE id=?; DROP TABLE mod_overviews; DROP INDEX ingest_mod_history; PRAGMA user_version=6;", id); err != nil {
		t.Fatal(err)
	}
	if err = migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	got, _ := a.readMod(id, 0)
	other, _ := a.readMod(manual.ID, 0)
	if got.CreatedAt != first.Source.Time || got.UpdatedAt != second.Source.Time || fingerprint(got) != fingerprint(before) || got.Revision != before.Revision || other.CreatedAt != manual.CreatedAt || other.UpdatedAt != manual.UpdatedAt {
		t.Fatal("migration damaged content/dates", got, other)
	}
	if err = migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = a.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
}
