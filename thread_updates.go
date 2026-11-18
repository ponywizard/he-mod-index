package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
)

type rowReader interface {
	QueryRow(string, ...any) *sql.Row
}

// This permission is independent of ownership: opting in never transfers a
// listing or grants its owner administrative privileges. Callers still check
// the importing admin, source identity, chronology, and content fingerprint.
func threadUpdatesAllowed(db rowReader, m mod, owner int64) (bool, error) {
	var allowed bool
	err := db.QueryRow("SELECT COALESCE((SELECT enabled FROM mod_thread_updates WHERE mod_id=?),?)", m.ID, owner > 0 && m.UserID == owner).Scan(&allowed)
	return allowed, err
}

type threadUpdateState struct {
	Enabled   bool   `json:"enabled"`
	HasSource bool   `json:"has_source"`
	Paused    bool   `json:"paused"`
	Reason    string `json:"reason"`
	ETag      string `json:"etag"`
}

func readThreadUpdateState(db rowReader, m mod, owner int64) (threadUpdateState, error) {
	var state threadUpdateState
	var err error
	state.Enabled, err = threadUpdatesAllowed(db, m, owner)
	if err != nil {
		return state, err
	}
	var post int64
	var sourceFingerprint string
	err = db.QueryRow("SELECT post_id,fingerprint FROM mod_sources WHERE mod_id=?", m.ID).Scan(&post, &sourceFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	state.HasSource = err == nil
	switch {
	case !state.HasSource:
		state.Reason = "Accept a current release with Update listing before following its thread updates."
	case owner == 0:
		state.Reason = "Automatic polling is not configured."
	case !state.Enabled:
		state.Reason = "Thread updates require manual review."
	case sourceFingerprint != fingerprint(m):
		state.Reason = "Paused after a manual edit. Review the next release with Update listing to resume."
	default:
		state.Reason = "Clear newer releases are accepted on the next poll. Ambiguous posts still need review."
	}
	state.Paused = state.Enabled && (!state.HasSource || owner == 0 || sourceFingerprint != fingerprint(m))
	// The content hash also detects description/image edits, which do not bump a
	// release revision. Include source and permission changes in stale-form checks.
	b, _ := json.Marshal([]any{m.ID, m.UserID, fingerprint(m), post, sourceFingerprint, state.Enabled, owner})
	state.ETag = digest(string(b))
	return state, nil
}

func (a *app) getThreadUpdates(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, id))
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Mod not found.")
	}
	if err != nil {
		return err
	}
	state, err := readThreadUpdateState(tx, m, a.cfg.IngestOptions.owner)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, state)
}

func (a *app) updateThreadUpdates(w http.ResponseWriter, r *http.Request) error {
	i, err := a.admin(r)
	if err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	var in struct {
		Enabled *bool  `json:"enabled"`
		ETag    string `json:"etag"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Enabled == nil || in.ETag == "" {
		return problem(400, "Choose whether to follow thread updates using the current setting.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, id))
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Mod not found.")
	}
	if err != nil {
		return err
	}
	state, err := readThreadUpdateState(tx, m, a.cfg.IngestOptions.owner)
	if err != nil {
		return err
	}
	if state.ETag != in.ETag {
		return problem(409, "Listing or thread-update setting changed. Reload and try again.")
	}
	if *in.Enabled && !state.HasSource {
		return problem(409, state.Reason)
	}
	if _, err = tx.Exec("INSERT INTO mod_thread_updates(mod_id,enabled) VALUES(?,?) ON CONFLICT(mod_id) DO UPDATE SET enabled=excluded.enabled", id, *in.Enabled); err != nil {
		return err
	}
	state, err = readThreadUpdateState(tx, m, a.cfg.IngestOptions.owner)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, 200, state)
}
