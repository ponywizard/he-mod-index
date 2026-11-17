package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type modImage struct {
	ImageURL     string `json:"image_url"`
	ThumbnailURL string `json:"thumbnail_url"`
	SourceURL    string `json:"source_url"`
	PostID       int64  `json:"post_id"`
	PostedAt     int64  `json:"posted_at"`
	Version      string `json:"version"`
	Description  string `json:"description"`
}

func (a *app) getModImages(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.authenticate(r); err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if _, err = a.readMod(id, 0); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT g.image_url,g.thumbnail_url,p.id,p.thread_id,p.posted_at,c.payload FROM mod_images g JOIN ingest_candidates c ON c.id=g.candidate_id AND c.mod_id=g.mod_id JOIN ingest_posts p ON p.id=c.post_id WHERE g.mod_id=? AND c.state='published' AND g.image_url<>'' ORDER BY p.posted_at DESC,p.id DESC,c.ordinal`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []modImage{}
	for rows.Next() {
		var im modImage
		var thread int64
		var payload string
		if err = rows.Scan(&im.ImageURL, &im.ThumbnailURL, &im.PostID, &thread, &im.PostedAt, &payload); err != nil {
			return err
		}
		if !validLocalImage(im.ImageURL, id) || im.ThumbnailURL != thumbnailURL(im.ImageURL) {
			continue
		}
		var entry extractedMod
		if err = json.Unmarshal([]byte(payload), &entry); err != nil {
			return err
		}
		im.SourceURL = fmt.Sprintf("https://desuarchive.org/mlp/thread/%d/#%d", thread, im.PostID)
		im.Version, im.Description = entry.Version, entry.ShortDescription
		list = append(list, im)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}

func (a *app) importCandidateImage(ctx context.Context, c importCandidate, modID, owner int64, actor *identity) error {
	return a.retainCandidateImage(ctx, c, modID, owner, actor, true)
}

// Gallery-only backfills do not change the index cover, descriptions, or dates.
// Publication stores the same local pair as both cover and historical image.
func (a *app) retainCandidateImage(ctx context.Context, c importCandidate, modID, owner int64, actor *identity, cover bool) error {
	if c.Entry.ImageURL == "" {
		return nil
	}
	// Extraction can select a demonstration clip; the image gallery accepts
	// still images only. Avoid downloading known videos just to reject them.
	if source, err := url.Parse(c.Entry.ImageURL); err == nil {
		switch strings.ToLower(filepath.Ext(source.Path)) {
		case ".mp4", ".webm", ".mov", ".avi", ".mkv":
			return nil
		}
	}
	var exists bool
	if err := a.db.QueryRow("SELECT EXISTS(SELECT 1 FROM mod_images WHERE candidate_id=?)", c.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	select {
	case a.imageSlots <- struct{}{}:
		defer func() { <-a.imageSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	old, err := a.readMod(modID, 0)
	if err != nil {
		return err
	}
	var beforeFingerprint string
	if err = a.db.QueryRow("SELECT fingerprint FROM mod_sources WHERE mod_id=?", modID).Scan(&beforeFingerprint); err != nil {
		return err
	}
	if beforeFingerprint != fingerprint(old) {
		if cover {
			return problem(409, "Listing was edited manually; review this image.")
		}
		// This also protects manual image removals made before gallery records
		// existed: there is no reliable source-image mapping to reconstruct.
		log.Printf("mod %d: skipping historical image after a manual edit", modID)
		return nil
	}
	img, err := a.remoteImage(ctx, c.Entry.ImageURL)
	if err != nil {
		return err
	}
	media, err := a.storeImage(modID, img, nil, nil)
	if err != nil {
		return err
	}
	createdURL := media.ImageURL
	keep := false
	defer func() {
		if !keep || media.ImageURL != createdURL {
			a.removeImage(createdURL, modID)
		}
	}()
	// Reuse an existing cover only when its bytes prove it is this source image.
	// A later source with a failed download must never inherit the wrong caption.
	if !cover && validLocalImage(old.ImageURL, modID) && old.ThumbnailURL == thumbnailURL(old.ImageURL) {
		oldBytes, oldErr := os.ReadFile(filepath.Join(a.cfg.DataDir, strings.TrimPrefix(old.ImageURL, "/")))
		newBytes, newErr := os.ReadFile(filepath.Join(a.cfg.DataDir, strings.TrimPrefix(media.ImageURL, "/")))
		if oldErr == nil && newErr == nil && bytes.Equal(oldBytes, newBytes) {
			media = storedImage{old.ImageURL, old.ThumbnailURL, old.ImageCrop}
		}
	}
	var tx *sql.Tx
	if actor != nil {
		tx, err = a.beginWrite(*actor)
	} else {
		tx, err = a.db.BeginTx(ctx, nil)
	}
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, modID))
	if err != nil {
		return err
	}
	var role string
	if err = tx.QueryRow("SELECT role FROM users WHERE id=?", owner).Scan(&role); err != nil {
		return err
	}
	if role != "admin" || actor == nil && current.UserID != owner {
		return problem(403, "Importer owner no longer authorized.")
	}
	if fingerprint(current) != fingerprint(old) {
		return problem(409, "Listing changed during image import.")
	}
	// The immutable published candidate is authoritative, including its image URL.
	var payload string
	if err = tx.QueryRow("SELECT payload FROM ingest_candidates WHERE id=? AND mod_id=? AND state='published' AND post_id=?", c.ID, modID, c.PostID).Scan(&payload); err != nil {
		return err
	}
	var accepted extractedMod
	if err = json.Unmarshal([]byte(payload), &accepted); err != nil {
		return err
	}
	if accepted.ImageURL != c.Entry.ImageURL {
		return problem(409, "Source changed during image import.")
	}
	var pid int64
	var fp string
	if err = tx.QueryRow("SELECT post_id,fingerprint FROM mod_sources WHERE mod_id=?", modID).Scan(&pid, &fp); err != nil {
		return err
	}
	if cover && pid != c.PostID || fp != fingerprint(current) {
		return problem(409, "Listing changed during image import.")
	}
	res, err := tx.Exec("INSERT INTO mod_images(candidate_id,mod_id,image_url,thumbnail_url) VALUES(?,?,?,?) ON CONFLICT(candidate_id) DO NOTHING", c.ID, modID, media.ImageURL, media.ThumbnailURL)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if cover {
		if _, err = tx.Exec("UPDATE mods SET image_url=?,thumbnail_url=?,crop_x=?,crop_y=?,crop_size=? WHERE id=?", media.ImageURL, media.ThumbnailURL, media.Crop.X, media.Crop.Y, media.Crop.Size, modID); err != nil {
			return err
		}
		current.ImageURL, current.ThumbnailURL, current.ImageCrop = media.ImageURL, media.ThumbnailURL, media.Crop
		if _, err = tx.Exec("UPDATE mod_sources SET fingerprint=? WHERE mod_id=?", fingerprint(current), modID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	keep = true
	if cover {
		a.removeUnusedImage(old.ImageURL, modID)
	}
	return nil
}

func (a *app) removeUnusedImage(path string, id int64) {
	var used bool
	err := a.db.QueryRow("SELECT EXISTS(SELECT 1 FROM mod_images WHERE mod_id=? AND image_url=? UNION ALL SELECT 1 FROM mods WHERE id=? AND image_url=?)", id, path, id, path).Scan(&used)
	if err == nil && !used {
		a.removeImage(path, id)
	}
}

func (a *app) backfillGallery(ctx context.Context, owner int64) error {
	rows, err := a.db.Query(`SELECT c.id,c.mod_id,m.user_id FROM ingest_candidates c JOIN mods m ON m.id=c.mod_id JOIN users u ON u.id=m.user_id LEFT JOIN mod_images g ON g.candidate_id=c.id WHERE c.state='published' AND u.role='admin' AND (?=0 OR m.user_id=?) AND g.candidate_id IS NULL ORDER BY c.post_id,c.ordinal`, owner, owner)
	if err != nil {
		return err
	}
	type pending struct{ candidate, mod, owner int64 }
	list := []pending{}
	for rows.Next() {
		var item pending
		if err = rows.Scan(&item.candidate, &item.mod, &item.owner); err != nil {
			rows.Close()
			return err
		}
		list = append(list, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	failed := 0
	for _, item := range list {
		if err = ctx.Err(); err != nil {
			return err
		}
		c, err := candidateFrom(a.db.QueryRow(candidateSelect+" WHERE c.id=?", item.candidate))
		if err != nil {
			return err
		}
		if err = a.retainCandidateImage(ctx, c, item.mod, item.owner, nil, false); err != nil {
			failed++
			log.Printf("mod %d image from post %d: %v", item.mod, c.PostID, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d source images could not be retained; successful images are saved, rerun ingest images to retry", failed)
	}
	return nil
}

// Collect paths before the FK cascade removes historical image records.
func modImagePaths(tx *sql.Tx, id int64) ([]string, error) {
	rows, err := tx.Query("SELECT image_url FROM mods WHERE id=? UNION SELECT image_url FROM mod_images WHERE mod_id=?", id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// Crop edits keep provenance; explicit removal/replacement suppresses the old
// gallery entry permanently so historical backfills cannot restore it.
func updateGalleryImage(tx *sql.Tx, id int64, old string, media storedImage, recrop bool) error {
	if !recrop {
		media = storedImage{}
	}
	_, err := tx.Exec("UPDATE mod_images SET image_url=?,thumbnail_url=? WHERE mod_id=? AND image_url=? AND image_url<>''", media.ImageURL, media.ThumbnailURL, id, old)
	return err
}
