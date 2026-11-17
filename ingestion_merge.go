package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
)

// Merge source work only. Accounts, credentials, ownership, approvals, votes and
// mod IDs in the staging database are never copied into the destination.
func (a *app) mergeIngestion(ctx context.Context, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join(a.cfg.DataDir, "index.db"))
	if err != nil {
		return err
	}
	sourceReal, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if sourceReal == targetReal {
		return errors.New("source and destination databases must differ")
	}
	sourceApp := &app{cfg: config{DataDir: filepath.Dir(sourceReal)}}
	unlock, err := sourceApp.ingestionLock()
	if err != nil {
		return fmt.Errorf("staging database: %w", err)
	}
	defer unlock()
	u := url.URL{Scheme: "file", Path: sourceReal}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	source, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer source.Close()
	source.SetMaxOpenConns(2)
	sourceApp.db = source
	var version int
	if err = source.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 6 || version > schemaVersion {
		return fmt.Errorf("staging schema %d is unsupported (maximum %d)", version, schemaVersion)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hash := sha256.New()
	rows, err := source.Query("SELECT id,thread_id,posted_at,payload,state,reason FROM ingest_posts ORDER BY id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, thread, posted int64
		var payload, state, reason string
		if err = rows.Scan(&id, &thread, &posted, &payload, &state, &reason); err != nil {
			rows.Close()
			return err
		}
		var p threadPost
		if err = json.Unmarshal([]byte(payload), &p); err != nil {
			rows.Close()
			return err
		}
		if p.ID != id || p.ThreadID != thread || p.Time != posted || id <= 0 || thread <= 0 || posted <= 0 {
			rows.Close()
			return errors.New("staging post identity mismatch")
		}
		fmt.Fprintf(hash, "post:%d:%s:%s:%s\n", id, state, reason, payload)
		_, err = tx.Exec("INSERT INTO ingest_posts(id,thread_id,posted_at,payload,state,reason) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,reason=excluded.reason WHERE ingest_posts.state='pending' AND ingest_posts.thread_id=excluded.thread_id AND ingest_posts.posted_at=excluded.posted_at", id, thread, posted, payload, state, reason)
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = source.Query("SELECT post_id,ordinal,payload,reason,state FROM ingest_candidates ORDER BY post_id,ordinal")
	if err != nil {
		return err
	}
	for rows.Next() {
		var postID int64
		var ordinal int
		var payload, reason, state string
		if err = rows.Scan(&postID, &ordinal, &payload, &reason, &state); err != nil {
			rows.Close()
			return err
		}
		var e extractedMod
		if err = json.Unmarshal([]byte(payload), &e); err != nil {
			rows.Close()
			return err
		}
		var raw string
		if err = source.QueryRow("SELECT payload FROM ingest_posts WHERE id=?", postID).Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var post threadPost
		if err = json.Unmarshal([]byte(raw), &post); err != nil {
			rows.Close()
			return err
		}
		contextPosts, err := sourceApp.postContext([]threadPost{post})
		if err != nil {
			rows.Close()
			return err
		}
		if err = validateExtraction(e, post, contextPosts); err != nil {
			rows.Close()
			return fmt.Errorf("staging candidate %d/%d: %w", postID, ordinal, err)
		}
		if state == "published" {
			state = "pending"
		}
		fmt.Fprintf(hash, "candidate:%d:%d:%s:%s:%s\n", postID, ordinal, state, reason, payload)
		if _, err = tx.Exec("INSERT OR IGNORE INTO ingest_candidates(post_id,ordinal,payload,reason,state) VALUES(?,?,?,?,?)", postID, ordinal, payload, reason, state); err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var cost int64
	if err = source.QueryRow("SELECT COALESCE(SUM(COALESCE(actual_micros,reserved_micros)),0) FROM ingest_usage").Scan(&cost); err != nil {
		return err
	}
	fmt.Fprint(hash, "cost:", cost)
	key := "ingest_merge:" + hex.EncodeToString(hash.Sum(nil))
	result, err := tx.Exec("INSERT OR IGNORE INTO settings(key,value) VALUES(?,?)", key, strconv.FormatInt(cost, 10))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n > 0 && cost > 0 {
		if _, err = tx.Exec("INSERT INTO ingest_usage(model,stage,reserved_micros,actual_micros,created_at) VALUES('staging','merge',?,?,unixepoch())", cost, cost); err != nil {
			return err
		}
	}
	return tx.Commit()
}
