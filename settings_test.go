package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnonymousDisplayName(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	account := owner.register(t, "hidden_author")
	if account.Anonymous {
		t.Fatal("new account unexpectedly anonymous")
	}
	m := owner.addMod(t, "Existing mod")
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "reviewer_account", "reviewer")
	status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
	visitor := client(a, "192.0.2.3")
	status(t, visitor.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, nil), 401)
	status(t, owner.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, map[string]string{"X-CSRF-Token": ""}), 403)
	status(t, owner.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, map[string]string{"Origin": "https://evil.example"}), 403)
	for _, body := range []map[string]any{{}, {"anonymous": nil}, {"anonymous": "yes"}, {"anonymous": true, "role": "admin"}, {"anonymous": true, "user_id": 99}} {
		status(t, owner.request("PUT", "/api/settings", body, nil), 400)
	}
	w := owner.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, nil)
	status(t, w, 200)
	updated := readJSON[user](t, w)
	if !updated.Anonymous || updated.Username != account.Username || updated.Role != "user" || updated.ID != account.ID {
		t.Fatal("settings changed account identity", updated)
	}
	checkPublic := func(c *testClient, author, approvedBy string) {
		t.Helper()
		for _, path := range []string{"/api/mods", modPath(m)} {
			response := c.request("GET", path, nil, nil)
			status(t, response, 200)
			if author == "Anonymous" && strings.Contains(response.Body.String(), account.Username) {
				t.Fatalf("public response leaked account username: %s", response.Body.String())
			}
			var got mod
			if path == "/api/mods" {
				list := readJSON[[]mod](t, response)
				for _, item := range list {
					if item.ID == m.ID {
						got = item
					}
				}
			} else {
				got = readJSON[mod](t, response)
			}
			if got.Author != author || got.ApprovedBy != approvedBy || got.UserID != account.ID || got.Approval != "current" || got.Revision != m.Revision {
				t.Fatalf("incorrect public attribution or changed release: %+v", got)
			}
		}
	}
	checkPublic(visitor, "Anonymous", "reviewer_account")
	w = owner.request("GET", "/api/session", nil, nil)
	status(t, w, 200)
	var self struct{ User user }
	if err := json.Unmarshal(w.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	if self.User.Username != account.Username || !self.User.Anonymous {
		t.Fatal("private session missing preference or login name")
	}
	newMod := owner.addMod(t, "New mod")
	if newMod.Author != "Anonymous" {
		t.Fatal("new mod exposed anonymous author's username")
	}
	w = owner.request("PUT", modPath(m), m.modInput, nil)
	status(t, w, 200)
	if readJSON[mod](t, w).Author != "Anonymous" {
		t.Fatal("update response exposed anonymous author's username")
	}
	status(t, visitor.request("POST", "/api/register", credentials{"other_author", "test-password-123"}, nil), 201)
	status(t, visitor.request("PUT", modPath(m), m.modInput, nil), 403)
	status(t, visitor.request("PUT", modPath(m)+"/vote", map[string]int{"value": 1}, nil), 200)
	w = owner.request("POST", "/api/tokens", map[string]string{"name": "Agent"}, nil)
	status(t, w, 201)
	var token struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &token)
	agent := client(a, "192.0.2.1")
	agent.token = token.Token
	status(t, agent.request("PUT", "/api/settings", map[string]bool{"anonymous": false}, nil), 403)
	checkPublic(agent, "Anonymous", "reviewer_account")
	status(t, reviewer.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, nil), 200)
	checkPublic(visitor, "Anonymous", "Anonymous")
	w = visitor.request("GET", modPath(m), nil, nil)
	if strings.Contains(w.Body.String(), "reviewer_account") {
		t.Fatal("anonymous review leaked reviewer username")
	}
	admin := client(a, "192.0.2.4")
	admin.provision(t, "ponywizard", "admin")
	w = admin.request("GET", "/api/admin/users", nil, nil)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), account.Username) {
		t.Fatal("moderation list hid account username")
	}
	status(t, owner.request("PUT", "/api/settings", map[string]bool{"anonymous": false}, nil), 200)
	checkPublic(visitor, account.Username, "Anonymous")
	if readJSON[mod](t, visitor.request("GET", modPath(newMod), nil, nil)).Author != account.Username {
		t.Fatal("setting did not apply to all historical mods")
	}
	status(t, reviewer.request("PUT", "/api/settings", map[string]bool{"anonymous": false}, nil), 200)
	checkPublic(visitor, account.Username, "reviewer_account")
	status(t, owner.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, nil), 200)
	status(t, owner.request("POST", "/api/logout", nil, nil), 200)
	if loggedIn := owner.login(t, account.Username); !loggedIn.Anonymous {
		t.Fatal("preference did not survive login")
	}
	if err := a.provisionUser(account.Username, "test-password-123", "user"); err != nil {
		t.Fatal(err)
	}
	if loggedIn := owner.login(t, account.Username); !loggedIn.Anonymous {
		t.Fatal("operator password reset cleared preference")
	}
}

func TestSettingsMigrationFromVersionTwo(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	account := owner.register(t, "existing_author")
	m := owner.addMod(t, "Before settings")
	// Reconstruct the preceding schema while retaining accounts, sessions, and mods.
	if _, err := a.db.Exec("ALTER TABLE users DROP COLUMN anonymous; PRAGMA user_version=2;"); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := a.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("settings migration did not advance schema", version, err)
	}
	w := owner.request("GET", "/api/session", nil, nil)
	status(t, w, 200)
	var self struct{ User user }
	json.Unmarshal(w.Body.Bytes(), &self)
	if self.User.Anonymous || self.User.Username != account.Username {
		t.Fatal("migration changed existing account settings")
	}
	get := func() *httptest.ResponseRecorder { return owner.request("GET", modPath(m), nil, nil) }
	got := readJSON[mod](t, get())
	if got.Author != account.Username || got.Revision != m.Revision {
		t.Fatal("migration changed existing mod")
	}
	status(t, owner.request("PUT", "/api/settings", map[string]bool{"anonymous": true}, nil), 200)
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if got = readJSON[mod](t, get()); got.Author != "Anonymous" {
		t.Fatal("restarting migration reset saved preference")
	}
}
