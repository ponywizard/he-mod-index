package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrateDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrateDB(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 3 {
		return fmt.Errorf("database schema %d is newer than this executable supports", version)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err = tx.Exec(schema); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		if _, err = tx.Exec(`ALTER TABLE mods ADD COLUMN downloads_json TEXT NOT NULL DEFAULT '[]';
		ALTER TABLE mods ADD COLUMN thumbnail_url TEXT NOT NULL DEFAULT '';
		ALTER TABLE mods ADD COLUMN crop_x INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE mods ADD COLUMN crop_y INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE mods ADD COLUMN crop_size INTEGER NOT NULL DEFAULT 0;`); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT id,download_url FROM mods")
		if err != nil {
			return err
		}
		type legacy struct {
			id  int64
			url string
		}
		old := []legacy{}
		for rows.Next() {
			var m legacy
			if err = rows.Scan(&m.id, &m.url); err != nil {
				rows.Close()
				return err
			}
			old = append(old, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, m := range old {
			data, err := json.Marshal([]download{{Filename: downloadFilename(m.url), URL: m.url}})
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE mods SET downloads_json=? WHERE id=?", string(data), m.id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("PRAGMA user_version=2"); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		if _, err = tx.Exec(`ALTER TABLE users ADD COLUMN anonymous INTEGER NOT NULL DEFAULT 0 CHECK(anonymous IN (0,1));
		PRAGMA user_version=3;`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY, username TEXT NOT NULL COLLATE NOCASE UNIQUE,
 password TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('user','reviewer','admin')),
 created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
 hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS mods (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name TEXT NOT NULL, version TEXT NOT NULL, game_version TEXT NOT NULL DEFAULT '',
 download_url TEXT NOT NULL, repository_url TEXT NOT NULL DEFAULT '',
 short_description TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', image_url TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 approved_revision INTEGER, approved_by INTEGER REFERENCES users(id) ON DELETE SET NULL, approved_at INTEGER
);
CREATE TABLE IF NOT EXISTS votes (
 mod_id INTEGER NOT NULL REFERENCES mods(id) ON DELETE CASCADE,
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 ip_hash TEXT NOT NULL, value INTEGER NOT NULL CHECK(value IN (-1,1)),
 PRIMARY KEY(mod_id,user_id), UNIQUE(mod_id,ip_hash)
);
CREATE TABLE IF NOT EXISTS activity (
 id INTEGER PRIMARY KEY, mod_id INTEGER NOT NULL REFERENCES mods(id) ON DELETE CASCADE,
 actor_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('release','vote')), created_at INTEGER NOT NULL,
 UNIQUE(mod_id,actor_id,kind)
);
CREATE INDEX IF NOT EXISTS activity_recent ON activity(created_at);
CREATE INDEX IF NOT EXISTS mods_owner ON mods(user_id);
CREATE TABLE IF NOT EXISTS limits (
 key TEXT PRIMARY KEY, count INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
PRAGMA user_version=1;
`
