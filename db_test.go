package main

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func legacyVersionFourDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schemaV1 + `
ALTER TABLE users ADD COLUMN anonymous INTEGER NOT NULL DEFAULT 0 CHECK(anonymous IN (0,1));
ALTER TABLE mods ADD COLUMN downloads_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE mods ADD COLUMN thumbnail_url TEXT NOT NULL DEFAULT '';
ALTER TABLE mods ADD COLUMN crop_x INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mods ADD COLUMN crop_y INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mods ADD COLUMN crop_size INTEGER NOT NULL DEFAULT 0;
DROP TABLE activity;
INSERT INTO users VALUES(7,'author','password-hash','user',100,1),(9,'reviewer','reviewer-hash','reviewer',101,0);
INSERT INTO sessions VALUES('session-hash',7,4102444800);
INSERT INTO tokens VALUES(17,7,'upload','token-hash',100,4102444800);
INSERT INTO mods(id,user_id,name,version,download_url,short_description,created_at,updated_at,
 revision,approved_revision,approved_by,approved_at,downloads_json)
 VALUES(13,7,'Existing mod','1.0','https://example.org/mod.pck','Description',100,200,3,3,9,250,
 '[{"filename":"mod.pck","url":"https://example.org/mod.pck"}]');
INSERT INTO votes VALUES(13,9,'ip-hash',1);
PRAGMA user_version=4;`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersionFiveMigrationPreservesDataAndCascades(t *testing.T) {
	path := legacyVersionFourDB(t)
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := newApp(db, config{DataDir: filepath.Dir(path), RegistrationCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.readMod(13, 9)
	if err != nil || m.UserID != 7 || m.Author != "Anonymous" || m.ApprovedBy != "reviewer" || m.Approval != "current" || m.Revision != 3 || m.Rating != 1 || m.MyVote != 1 || len(m.Downloads) != 1 {
		t.Fatalf("migration lost listing data: %+v, %v", m, err)
	}
	var password string
	if err = db.QueryRow("SELECT password FROM users WHERE id=7").Scan(&password); err != nil || password != "password-hash" {
		t.Fatal("migration lost password", err)
	}
	for _, table := range []string{"sessions", "tokens"} {
		var count int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE user_id=7").Scan(&count); err != nil || count != 1 {
			t.Fatal("migration lost credentials", table, count, err)
		}
	}
	if err = migrateDB(db); err != nil {
		t.Fatal(err)
	}
	next, err := a.readMod(13, 9)
	if err != nil || !reflect.DeepEqual(m, next) {
		t.Fatal("repeat migration changed listing", next, err)
	}
	if _, err = db.Exec("DELETE FROM users WHERE id=9"); err != nil {
		t.Fatal(err)
	}
	m, err = a.readMod(13, 0)
	if err != nil || m.Approval != "none" || m.Rating != 0 {
		t.Fatal("reviewer deletion did not clear approval and votes", m, err)
	}
	// A rolled-back deletion must restore all dependent rows too.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("DELETE FROM users WHERE id=7"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = a.readMod(13, 0); err != nil {
		t.Fatal("cascade did not roll back", err)
	}
	if _, err = db.Exec("DELETE FROM users WHERE id=7"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mods", "votes", "sessions", "tokens"} {
		var count int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("cascade failed", table, count, err)
		}
	}
	// High-water marks must survive deleting every row and reopening the DB.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, item := range []struct {
		query   string
		minimum int64
	}{
		{"INSERT INTO users(username,password,created_at) VALUES('next','hash',300)", 9},
		{"INSERT INTO mods(user_id,name,version,download_url,short_description,created_at,updated_at) VALUES((SELECT id FROM users),'Next','2','https://example.org/next','Next',300,300)", 13},
		{"INSERT INTO tokens(user_id,name,hash,created_at,expires_at) VALUES((SELECT id FROM users),'next','next-token',300,4102444800)", 17},
	} {
		result, err := db.Exec(item.query)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil || id <= item.minimum {
			t.Fatal("deleted ID reused after restart", id, item.minimum, err)
		}
	}
}

func TestVersionFiveMigrationFailureRollsBack(t *testing.T) {
	path := legacyVersionFourDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	// A corrupt legacy reference must abort the rebuild without losing data.
	if _, err = db.Exec("UPDATE votes SET user_id=999; PRAGMA foreign_keys=ON;"); err != nil {
		t.Fatal(err)
	}
	if err = migrateDB(db); err == nil {
		t.Fatal("accepted invalid foreign key")
	}
	var version, enabled, count int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatal(version, err)
	}
	if err = db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatal("foreign keys not restored", enabled, err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM mods WHERE id=13").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed migration lost data", count, err)
	}
	if _, err = db.Exec("UPDATE votes SET user_id=9"); err != nil {
		t.Fatal(err)
	}
	if err = migrateDB(db); err != nil {
		t.Fatal("cannot retry repaired database", err)
	}
}
