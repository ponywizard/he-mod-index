package main

import (
	"context"
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

const schemaVersion = 6

func migrateDB(db *sql.DB) (err error) {
	// Table rebuilds require foreign_keys to be disabled before the transaction.
	// Pin the connection so enforcement is restored on the same connection on
	// success and rollback, before it is returned to the pool.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than this executable supports", version)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); restoreErr != nil {
			err = fmt.Errorf("restore foreign key enforcement: %w (migration: %v)", restoreErr, err)
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err = tx.Exec(schemaV1); err != nil {
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
		version = 3
	}
	if version == 3 {
		if _, err = tx.Exec("DROP TABLE IF EXISTS activity; PRAGMA user_version=4;"); err != nil {
			return err
		}
		version = 4
	}
	if version == 4 {
		if _, err = tx.Exec(schemaV5); err != nil {
			return err
		}
		version = 5
	}
	if version == 5 {
		if _, err = tx.Exec(schemaV6); err != nil {
			return err
		}
	}
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	invalid := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if invalid {
		return fmt.Errorf("migration would leave invalid foreign key references")
	}
	return tx.Commit()
}

// Source records are independent of accounts. Candidate publication and mod
// provenance are committed together; deletion cannot resurrect an old import.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS ingest_posts (
 id INTEGER PRIMARY KEY, thread_id INTEGER NOT NULL, posted_at INTEGER NOT NULL,
 payload TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending'
 CHECK(state IN ('pending','other','candidate','done')), reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ingest_pending ON ingest_posts(state,id);
CREATE TABLE IF NOT EXISTS ingest_candidates (
 id INTEGER PRIMARY KEY AUTOINCREMENT, post_id INTEGER NOT NULL REFERENCES ingest_posts(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL, payload TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','published','rejected')),
 mod_id INTEGER REFERENCES mods(id) ON DELETE SET NULL, UNIQUE(post_id,ordinal)
);
CREATE TABLE IF NOT EXISTS mod_sources (
 mod_id INTEGER PRIMARY KEY REFERENCES mods(id) ON DELETE CASCADE,
 post_id INTEGER NOT NULL REFERENCES ingest_posts(id), author TEXT NOT NULL,
 source_url TEXT NOT NULL, posted_at INTEGER NOT NULL, fingerprint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ingest_usage (
 id INTEGER PRIMARY KEY AUTOINCREMENT, model TEXT NOT NULL, stage TEXT NOT NULL,
 reserved_micros INTEGER NOT NULL, actual_micros INTEGER, created_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO settings(key,value) VALUES('current_thread','43715264');
PRAGMA user_version=6;
`

// AUTOINCREMENT retains the highest committed ID even after deletion. Preserve
// existing IDs and references; never rename the old tables before rebuilding.
const schemaV5 = `
CREATE TABLE users_v5 (
 id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL COLLATE NOCASE UNIQUE,
 password TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('user','reviewer','admin')),
 created_at INTEGER NOT NULL, anonymous INTEGER NOT NULL DEFAULT 0 CHECK(anonymous IN (0,1))
);
INSERT INTO users_v5 SELECT id,username,password,role,created_at,anonymous FROM users;
DROP TABLE users;
ALTER TABLE users_v5 RENAME TO users;

CREATE TABLE mods_v5 (
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name TEXT NOT NULL, version TEXT NOT NULL, game_version TEXT NOT NULL DEFAULT '',
 download_url TEXT NOT NULL, repository_url TEXT NOT NULL DEFAULT '',
 short_description TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', image_url TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 approved_revision INTEGER, approved_by INTEGER REFERENCES users(id) ON DELETE SET NULL, approved_at INTEGER,
 downloads_json TEXT NOT NULL DEFAULT '[]', thumbnail_url TEXT NOT NULL DEFAULT '',
 crop_x INTEGER NOT NULL DEFAULT 0, crop_y INTEGER NOT NULL DEFAULT 0, crop_size INTEGER NOT NULL DEFAULT 0
);
INSERT INTO mods_v5 SELECT id,user_id,name,version,game_version,download_url,repository_url,
 short_description,description,image_url,created_at,updated_at,revision,approved_revision,approved_by,approved_at,
 downloads_json,thumbnail_url,crop_x,crop_y,crop_size FROM mods;
DROP TABLE mods;
ALTER TABLE mods_v5 RENAME TO mods;
CREATE INDEX mods_owner ON mods(user_id);

CREATE TABLE tokens_v5 (
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
INSERT INTO tokens_v5 SELECT id,user_id,name,hash,created_at,expires_at FROM tokens;
DROP TABLE tokens;
ALTER TABLE tokens_v5 RENAME TO tokens;
PRAGMA user_version=5;
`

// Keep the original schema as the migration baseline for older installations.
const schemaV1 = `
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
