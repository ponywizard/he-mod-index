package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Source attachments are separate from a mod's chosen cover/gallery. Previewing
// one does not approve it as a screenshot or modify the extracted announcement.
func (a *app) sourcePostImage(w http.ResponseWriter, r *http.Request) error {
	i, err := a.authenticate(r)
	if err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if i.User == nil || i.User.Role != "admin" || i.Bearer {
		var published bool
		if err = a.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM ingest_candidates c JOIN mods m ON m.id=c.mod_id WHERE c.post_id=? AND c.state='published')`, id).Scan(&published); err != nil {
			return err
		}
		if !published {
			return problem(404, "Source image not found.")
		}
	}
	var raw string
	err = a.db.QueryRow("SELECT payload FROM ingest_posts WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Source image not found.")
	}
	if err != nil {
		return err
	}
	var post threadPost
	if err = json.Unmarshal([]byte(raw), &post); err != nil {
		return err
	}
	if post.Image == "" {
		return problem(404, "Post has no attached image.")
	}
	if _, err = imageURL(post.Image); err != nil {
		return err
	}
	directory := filepath.Join(a.cfg.DataDir, "source-images")
	path := filepath.Join(directory, fmt.Sprintf("%d-%s.jpg", id, digest(post.Image)))
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err = a.limit("source-image:"+a.keyed(a.clientIP(r)), 120, time.Hour); err != nil {
			return err
		}
		select {
		case a.imageSlots <- struct{}{}:
			defer func() { <-a.imageSlots }()
		case <-r.Context().Done():
			return r.Context().Err()
		}
		im, err := a.remoteImage(r.Context(), post.Image)
		if err != nil {
			return err
		}
		data, err := encodeImage(flattenImage(im))
		if err != nil {
			return err
		}
		if err = os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		temporary, err := os.CreateTemp(directory, ".source-*")
		if err != nil {
			return err
		}
		defer os.Remove(temporary.Name())
		_, err = temporary.Write(data)
		closeErr := temporary.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(temporary.Name(), path); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Add("Vary", "Cookie")
	http.ServeFile(w, r, path)
	return nil
}
