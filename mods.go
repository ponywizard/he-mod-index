package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type modInput struct {
	Name             string     `json:"name"`
	Version          string     `json:"version"`
	GameVersion      string     `json:"game_version"`
	DownloadURL      string     `json:"download_url"`
	Downloads        []download `json:"downloads"`
	RepositoryURL    string     `json:"repository_url"`
	ShortDescription string     `json:"short_description"`
	Description      string     `json:"description"`
	ImageURL         string     `json:"image_url"`
}
type mod struct {
	modInput
	ID           int64    `json:"id"`
	UserID       int64    `json:"user_id"`
	Author       string   `json:"author"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
	Revision     int64    `json:"revision"`
	Approval     string   `json:"approval"`
	ApprovedBy   string   `json:"approved_by"`
	ApprovedAt   int64    `json:"approved_at"`
	Rating       int      `json:"rating"`
	Upvotes      int      `json:"upvotes"`
	Downvotes    int      `json:"downvotes"`
	MyVote       int      `json:"my_vote"`
	ThumbnailURL string   `json:"thumbnail_url"`
	ImageCrop    cropRect `json:"image_crop"`
	SourceURL    string   `json:"source_url"`
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && len(s) <= 2048
}
func (in *modInput) validate(id int64) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Version = strings.TrimSpace(in.Version)
	in.GameVersion = strings.TrimSpace(in.GameVersion)
	in.ShortDescription = strings.TrimSpace(in.ShortDescription)
	in.DownloadURL = strings.TrimSpace(in.DownloadURL)
	in.RepositoryURL = strings.TrimSpace(in.RepositoryURL)
	in.ImageURL = strings.TrimSpace(in.ImageURL)
	if len(in.Name) < 1 || len(in.Name) > 100 {
		return problem(400, "Name must be 1–100 bytes.")
	}
	if len(in.Version) < 1 || len(in.Version) > 64 || len(in.GameVersion) > 64 {
		return problem(400, "Version is required (up to 64 bytes).")
	}
	if err := in.validateDownloads(); err != nil {
		return err
	}
	if in.RepositoryURL != "" && !validURL(in.RepositoryURL) {
		return problem(400, "Invalid repository URL.")
	}
	if len(in.ShortDescription) < 1 || len(in.ShortDescription) > 240 {
		return problem(400, "Short description must be 1–240 bytes.")
	}
	if len(in.Description) > 20000 {
		return problem(400, "Description is limited to 20,000 bytes.")
	}
	if in.ImageURL != "" && !validURL(in.ImageURL) {
		if id < 1 || !validLocalImage(in.ImageURL, id) {
			return problem(400, "Invalid image URL. Use http(s) or upload an image.")
		}
	}
	return nil
}

const modSelect = `SELECT m.id,m.user_id,CASE WHEN u.anonymous=1 THEN 'Anonymous' ELSE COALESCE((SELECT author FROM mod_sources WHERE mod_id=m.id),u.username) END,m.name,m.version,m.game_version,m.download_url,m.downloads_json,m.repository_url,m.short_description,m.description,m.image_url,m.thumbnail_url,m.crop_x,m.crop_y,m.crop_size,m.created_at,m.updated_at,m.revision,
 CASE WHEN m.approved_by IS NULL THEN 'none' WHEN m.approved_revision=m.revision THEN 'current' ELSE 'stale' END,
 CASE WHEN reviewer.id IS NULL THEN '' WHEN reviewer.anonymous=1 THEN 'Anonymous' ELSE reviewer.username END,COALESCE(m.approved_at,0),
 COALESCE((SELECT SUM(value) FROM votes WHERE mod_id=m.id),0),
 (SELECT COUNT(*) FROM votes WHERE mod_id=m.id AND value=1),
 (SELECT COUNT(*) FROM votes WHERE mod_id=m.id AND value=-1),
 COALESCE((SELECT value FROM votes WHERE mod_id=m.id AND user_id=?),0),
 COALESCE((SELECT source_url FROM mod_sources WHERE mod_id=m.id),'')
 FROM mods m JOIN users u ON u.id=m.user_id LEFT JOIN users reviewer ON reviewer.id=m.approved_by `

type scanner interface{ Scan(...any) error }

func scanMod(s scanner) (mod, error) {
	var m mod
	var downloads string
	err := s.Scan(&m.ID, &m.UserID, &m.Author, &m.Name, &m.Version, &m.GameVersion, &m.DownloadURL, &downloads, &m.RepositoryURL, &m.ShortDescription, &m.Description, &m.ImageURL, &m.ThumbnailURL, &m.ImageCrop.X, &m.ImageCrop.Y, &m.ImageCrop.Size, &m.CreatedAt, &m.UpdatedAt, &m.Revision, &m.Approval, &m.ApprovedBy, &m.ApprovedAt, &m.Rating, &m.Upvotes, &m.Downvotes, &m.MyVote, &m.SourceURL)
	if err == nil {
		err = json.Unmarshal([]byte(downloads), &m.Downloads)
	}
	return m, err
}
func (a *app) readMod(id, viewer int64) (mod, error) {
	m, err := scanMod(a.db.QueryRow(modSelect+" WHERE m.id=?", viewer, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, problem(404, "Mod not found.")
	}
	return m, err
}
func (a *app) listMods(w http.ResponseWriter, r *http.Request) error {
	i, err := a.authenticate(r)
	if err != nil {
		return err
	}
	var uid int64
	if i.User != nil {
		uid = i.User.ID
	}
	rows, err := a.db.Query(modSelect+" ORDER BY m.updated_at DESC,m.id DESC", uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []mod{}
	for rows.Next() {
		m, err := scanMod(rows)
		if err != nil {
			return err
		}
		list = append(list, m)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}
func (a *app) getMod(w http.ResponseWriter, r *http.Request) error {
	i, err := a.authenticate(r)
	if err != nil {
		return err
	}
	var uid int64
	if i.User != nil {
		uid = i.User.ID
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	m, err := a.readMod(id, uid)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}
func (a *app) createMod(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, false)
	if err != nil {
		return err
	}
	if err = a.limit("mod-create:"+a.keyed(a.clientIP(r)), 20, time.Hour); err != nil {
		return err
	}
	var in modInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(0); err != nil {
		return err
	}
	var imported image.Image
	if in.ImageURL != "" {
		release, err := a.imageJob(r)
		if err != nil {
			return err
		}
		defer release()
		imported, err = a.remoteImage(r.Context(), in.ImageURL)
		if err != nil {
			return err
		}
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	downloads, _ := json.Marshal(in.Downloads)
	res, err := tx.Exec(`INSERT INTO mods(user_id,name,version,game_version,download_url,downloads_json,repository_url,short_description,description,image_url,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, i.User.ID, in.Name, in.Version, in.GameVersion, in.DownloadURL, string(downloads), in.RepositoryURL, in.ShortDescription, in.Description, "", now, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	media := storedImage{}
	defer func() {
		if media.ImageURL != "" {
			a.removeImage(media.ImageURL, id)
		}
	}()
	if imported != nil {
		media, err = a.storeImage(id, imported, nil, nil)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE mods SET image_url=?,thumbnail_url=?,crop_x=?,crop_y=?,crop_size=? WHERE id=?", media.ImageURL, media.ThumbnailURL, media.Crop.X, media.Crop.Y, media.Crop.Size, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	media.ImageURL = ""
	m, err := a.readMod(id, i.User.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, m)
}
func (a *app) ownedMod(r *http.Request) (identity, mod, error) {
	i, err := a.require(r, false)
	if err != nil {
		return i, mod{}, err
	}
	id, err := idParam(r)
	if err != nil {
		return i, mod{}, err
	}
	m, err := a.readMod(id, i.User.ID)
	if err != nil {
		return i, m, err
	}
	// Tokens always stay within the owner's mods, including tokens held by admins.
	if m.UserID != i.User.ID && (i.User.Role != "admin" || i.Bearer) {
		return i, m, problem(403, "You do not own this mod.")
	}
	return i, m, nil
}
func (a *app) updateMod(w http.ResponseWriter, r *http.Request) error {
	i, old, err := a.ownedMod(r)
	if err != nil {
		return err
	}
	if err = a.limit("mod-update:"+a.keyed(a.clientIP(r)), 120, time.Hour); err != nil {
		return err
	}
	var in modInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Downloads == nil && len(old.Downloads) > 1 {
		return problem(400, "Use the downloads array to preserve all files in this mod.")
	}
	if err = in.validate(old.ID); err != nil {
		return err
	}
	if strings.HasPrefix(in.ImageURL, "/uploads/") && in.ImageURL != old.ImageURL {
		return problem(400, "Upload a new image using the image endpoint.")
	}
	media := storedImage{ImageURL: in.ImageURL}
	if in.ImageURL != "" && !strings.HasPrefix(in.ImageURL, "/uploads/") {
		media, err = a.importModImage(r, in.ImageURL, old.ID)
		if err != nil {
			return err
		}
		defer func() {
			if media.ImageURL != "" {
				a.removeImage(media.ImageURL, old.ID)
			}
		}()
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, old.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return problem(409, "Mod changed. Reload and try again.")
	}
	if err != nil {
		return err
	}
	if fingerprint(current) != fingerprint(old) {
		return problem(409, "Mod changed. Reload and try again.")
	}
	updatedAt := time.Now().Unix()
	if old.SourceURL != "" {
		updatedAt = old.UpdatedAt
	}
	release := old.Version != in.Version || !sameReleaseDownloads(old.Downloads, in.Downloads)
	var bump int
	if release {
		bump = 1
	}
	downloads, _ := json.Marshal(in.Downloads)
	res, err := tx.Exec(`UPDATE mods SET name=?,version=?,game_version=?,download_url=?,downloads_json=?,repository_url=?,short_description=?,description=?,image_url=?,updated_at=?,revision=revision+? WHERE id=? AND user_id=? AND revision=? AND updated_at=? AND image_url=?`, in.Name, in.Version, in.GameVersion, in.DownloadURL, string(downloads), in.RepositoryURL, in.ShortDescription, in.Description, media.ImageURL, updatedAt, bump, old.ID, old.UserID, old.Revision, old.UpdatedAt, old.ImageURL)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return problem(409, "Mod changed. Reload and try again.")
	}
	if media.ImageURL != old.ImageURL {
		if _, err = tx.Exec("UPDATE mods SET thumbnail_url=?,crop_x=?,crop_y=?,crop_size=? WHERE id=?", media.ThumbnailURL, media.Crop.X, media.Crop.Y, media.Crop.Size, old.ID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if old.ImageURL != media.ImageURL {
		a.removeImage(old.ImageURL, old.ID)
	}
	media.ImageURL = ""
	m, err := a.readMod(old.ID, i.User.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}
func (a *app) deleteMod(w http.ResponseWriter, r *http.Request) error {
	i, m, err := a.ownedMod(r)
	if err != nil {
		return err
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var imageURL string
	if err = tx.QueryRow("DELETE FROM mods WHERE id=? AND user_id=? RETURNING image_url", m.ID, m.UserID).Scan(&imageURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return problem(404, "Mod not found.")
		}
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.removeImage(imageURL, m.ID)
	return writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *app) vote(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if err = a.limit("vote:"+a.keyed(a.clientIP(r)), 120, time.Hour); err != nil {
		return err
	}
	var in struct {
		Value *int `json:"value"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Value == nil || *in.Value < -1 || *in.Value > 1 {
		return problem(400, "Vote must be -1, 0, or 1.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow("SELECT 1 FROM mods WHERE id=?", id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Mod not found.")
	}
	if err != nil {
		return err
	}
	if *in.Value == 0 {
		if _, err = tx.Exec("DELETE FROM votes WHERE mod_id=? AND user_id=?", id, i.User.ID); err != nil {
			return err
		}
	} else {
		_, err = tx.Exec(`INSERT INTO votes(mod_id,user_id,ip_hash,value) VALUES(?,?,?,?) ON CONFLICT(mod_id,user_id) DO UPDATE SET ip_hash=excluded.ip_hash,value=excluded.value`, id, i.User.ID, a.keyed("ip:"+a.clientIP(r)), *in.Value)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return problem(409, "This network already has a vote on this mod.")
			}
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m, err := a.readMod(id, i.User.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}
func (a *app) approve(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if i.User.Role != "reviewer" {
		return problem(403, "Reviewer access required.")
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	var in struct {
		Approved *bool `json:"approved"`
		Revision int64 `json:"revision"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Approved == nil {
		return problem(400, "approved is required.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var res sql.Result
	if *in.Approved {
		res, err = tx.Exec("UPDATE mods SET approved_revision=revision,approved_by=?,approved_at=? WHERE id=? AND revision=?", i.User.ID, time.Now().Unix(), id, in.Revision)
	} else {
		res, err = tx.Exec("UPDATE mods SET approved_revision=NULL,approved_by=NULL,approved_at=NULL WHERE id=? AND revision=?", id, in.Revision)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return problem(409, "Mod changed or was removed. Reload before reviewing.")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m, err := a.readMod(id, i.User.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}
