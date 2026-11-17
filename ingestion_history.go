package main

import (
	"context"
	"database/sql"
	"errors"
)

// History acceptance does not select a new current release or grant automatic
// update permission over a manually maintained listing.
func (a *app) acceptCandidateHistory(ctx context.Context, id, target, revision int64, actor identity) error {
	if actor.User == nil || actor.User.Role != "admin" || actor.Bearer {
		return problem(403, "Admin session required.")
	}
	tx, err := a.beginWrite(actor)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := candidateFrom(tx.QueryRowContext(ctx, candidateSelect+" WHERE c.id=? AND c.state='pending'", id))
	if errors.Is(err, sql.ErrNoRows) {
		return problem(409, "Candidate already handled or removed.")
	}
	if err != nil {
		return err
	}
	if excludedRelease(c.Entry) {
		return problem(409, "Base game or index release is outside the mod catalog.")
	}
	in := c.Entry.input()
	in.ImageURL = ""
	if err = in.validate(0); err != nil {
		return err
	}
	m, err := scanMod(tx.QueryRowContext(ctx, modSelect+" WHERE m.id=?", 0, target))
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Mod not found.")
	}
	if err != nil {
		return err
	}
	if m.Revision != revision {
		return problem(409, "Listing changed. Reload before adding history.")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE ingest_candidates SET state='published',mod_id=? WHERE id=?", target, id); err != nil {
		return err
	}
	if err = refreshSourceDates(ctx, tx, target); err != nil {
		return err
	}
	// Older entries must participate in the next full synthesis, even when the
	// latest release pointer has not changed. Manual-description checks still apply.
	if _, err = tx.ExecContext(ctx, "DELETE FROM mod_overviews WHERE mod_id=?", target); err != nil {
		return err
	}
	return tx.Commit()
}

func refreshSourceDates(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE mods SET
	created_at=(SELECT MIN(p.posted_at) FROM ingest_candidates c JOIN ingest_posts p ON p.id=c.post_id WHERE c.mod_id=? AND c.state='published'),
	updated_at=(SELECT MAX(p.posted_at) FROM ingest_candidates c JOIN ingest_posts p ON p.id=c.post_id WHERE c.mod_id=? AND c.state='published')
	WHERE id=?`, id, id, id)
	return err
}
