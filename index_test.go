package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoveActivityMigrationPreservesListingsAndVotes(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	account := owner.register(t, "index_author")
	m := owner.addMod(t, "Keep this mod")
	status(t, owner.request("PUT", modPath(m)+"/vote", map[string]int{"value": 1}, nil), 200)
	// Reconstruct the retired version-three activity table in an isolated database.
	if _, err := a.db.Exec(`CREATE TABLE activity (
	 id INTEGER PRIMARY KEY, mod_id INTEGER NOT NULL REFERENCES mods(id) ON DELETE CASCADE,
	 actor_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	 kind TEXT NOT NULL, created_at INTEGER NOT NULL, UNIQUE(mod_id,actor_id,kind)
	); CREATE INDEX activity_recent ON activity(created_at);
	INSERT INTO activity(mod_id,actor_id,kind,created_at) VALUES(?,?,'vote',123);
	PRAGMA user_version=3;`, m.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	var version, tables int
	if err := a.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("migration did not advance schema", version, err)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('activity','activity_recent')").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("retired activity storage remains", tables, err)
	}
	w := owner.request("GET", modPath(m), nil, nil)
	status(t, w, 200)
	got := readJSON[mod](t, w)
	if got.Name != m.Name || got.Author != account.Username || got.Rating != 1 || got.MyVote != 1 || got.Revision != m.Revision {
		t.Fatal("migration changed mod or votes", got)
	}
	for _, path := range []string{"/api/mods", modPath(m)} {
		response := owner.request("GET", path, nil, nil)
		status(t, response, 200)
		if strings.Contains(response.Body.String(), `"hot"`) {
			t.Fatal("retired metric is still public", response.Body.String())
		}
	}
	status(t, owner.request("PUT", modPath(m), m.modInput, nil), 200)
	w = owner.request("PUT", modPath(m)+"/vote", map[string]int{"value": 0}, nil)
	status(t, w, 200)
	if readJSON[mod](t, w).Rating != 0 {
		t.Fatal("vote removal depended on retired activity storage")
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal("migration is not repeatable", err)
	}
	status(t, owner.request("DELETE", modPath(m), nil, nil), 200)
}

func TestModResponsesHaveNoHotMetric(t *testing.T) {
	a := testApp(t)
	c := client(a, "192.0.2.1")
	c.register(t, "author")
	w := c.request("POST", "/api/mods", sampleMod("New mod"), nil)
	status(t, w, 201)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["hot"]; exists {
		t.Fatal("creation response includes retired metric")
	}
}
