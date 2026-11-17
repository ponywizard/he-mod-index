package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

func (a *app) savePosts(ctx context.Context, posts []threadPost) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range posts {
		if p.ID <= 0 || p.ThreadID <= 0 || p.Time <= 0 {
			return errors.New("source post lacks identity or timestamp")
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO ingest_posts(id,thread_id,posted_at,payload) VALUES(?,?,?,?)", p.ID, p.ThreadID, p.Time, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (a *app) postContext(posts []threadPost) ([]threadPost, error) {
	seen := map[int64]bool{}
	for _, p := range posts {
		seen[p.ID] = true
	}
	queue := []int64{}
	for _, p := range posts {
		queue = append(queue, p.Replies...)
	}
	out := []threadPost{}
	size := 0
	for len(queue) > 0 && len(out) < 30 && size < 60000 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		var b string
		err := a.db.QueryRow("SELECT payload FROM ingest_posts WHERE id=?", id).Scan(&b)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if size+len(b) > 60000 {
			continue
		}
		var p threadPost
		if err = json.Unmarshal([]byte(b), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
		size += len(b)
		queue = append(queue, p.Replies...)
	}
	return out, nil
}
func (a *app) pendingPosts(state string, limit int) ([]threadPost, error) {
	rows, err := a.db.Query("SELECT payload FROM ingest_posts WHERE state=? AND reason NOT LIKE 'Extraction needs retry:%' ORDER BY id LIMIT ?", state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []threadPost{}
	size := 0
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if size+len(b) > 70000 && len(out) > 0 {
			break
		}
		var p threadPost
		if err = json.Unmarshal([]byte(b), &p); err != nil {
			return nil, err
		}
		size += len(b)
		out = append(out, p)
	}
	return out, rows.Err()
}
func (a *app) classifyPending(ctx context.Context, c *modelClient) error {
	for {
		posts, err := a.pendingPosts("pending", 60)
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			return nil
		}
		contextPosts, err := a.postContext(posts)
		if err != nil {
			return err
		}
		result, err := c.classify(ctx, posts, contextPosts)
		if err != nil {
			return err
		}
		chosen := map[int64]classifiedPost{}
		for _, r := range result.Candidates {
			chosen[r.PostID] = r
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, p := range posts {
			state, reason := "other", ""
			if r, ok := chosen[p.ID]; ok {
				state = "candidate"
				reason = r.Kind + ": " + r.Reason
			}
			if _, err = tx.Exec("UPDATE ingest_posts SET state=?,reason=? WHERE id=? AND state='pending'", state, reason, p.ID); err != nil {
				break
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		log.Printf("classified through post %d: %d candidates in %d posts", posts[len(posts)-1].ID, len(result.Candidates), len(posts))
	}
}
func (a *app) extractPending(ctx context.Context, c *modelClient) error {
	for {
		posts, err := a.pendingPosts("candidate", 3)
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			return nil
		}
		results := make(chan error, len(posts))
		for _, p := range posts {
			go func() { results <- a.extractPost(ctx, c, p) }()
		}
		var first error
		for range posts {
			if err := <-results; err != nil && first == nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
	}
}
func (a *app) extractPost(ctx context.Context, c *modelClient, p threadPost) error {
	contextPosts, err := a.postContext([]threadPost{p})
	if err != nil {
		return err
	}
	entries, err := c.extract(ctx, p, contextPosts)
	if err != nil {
		var invalid *modelDataError
		if !errors.As(err, &invalid) {
			return fmt.Errorf("post %d: %w", p.ID, err)
		}
		if _, err = a.db.Exec("UPDATE ingest_posts SET reason=? WHERE id=?", "Extraction needs retry: "+invalid.Error(), p.ID); err != nil {
			return err
		}
		log.Printf("post %d held: %v", p.ID, invalid)
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var classificationReason string
	if err = tx.QueryRow("SELECT reason FROM ingest_posts WHERE id=?", p.ID).Scan(&classificationReason); err != nil {
		return err
	}
	for index, e := range entries {
		if strings.HasPrefix(classificationReason, "uncertain:") {
			e.Confidence = "uncertain"
			e.Reason = classificationReason
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO ingest_candidates(post_id,ordinal,payload,reason) VALUES(?,?,?,?)", p.ID, index, string(b), e.Reason); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE ingest_posts SET state='done' WHERE id=?", p.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(entries) > 0 {
		log.Printf("post %d: %d extracted entries", p.ID, len(entries))
	}
	return nil
}

type importCandidate struct {
	ID      int64        `json:"id"`
	PostID  int64        `json:"post_id"`
	Entry   extractedMod `json:"entry"`
	Reason  string       `json:"reason"`
	Source  threadPost   `json:"source"`
	Context []threadPost `json:"context,omitempty"`
}

func candidateFrom(s scanner) (importCandidate, error) {
	var c importCandidate
	var payload, source string
	err := s.Scan(&c.ID, &c.PostID, &payload, &c.Reason, &source)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(payload), &c.Entry); err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(source), &c.Source)
	return c, err
}

const candidateSelect = `SELECT c.id,c.post_id,c.payload,c.reason,p.payload FROM ingest_candidates c JOIN ingest_posts p ON p.id=c.post_id `

func fingerprint(m mod) string {
	b, _ := json.Marshal(struct {
		Content   modInput
		Thumbnail string
		Crop      cropRect
	}{m.modInput, m.ThumbnailURL, m.ImageCrop})
	return digest(string(b))
}
func normalizedName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, s)
	s = strings.TrimPrefix(strings.Join(strings.Fields(s), " "), "home equestria ")
	return strings.TrimSuffix(s, " mod")
}
func repositoryKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	if (host == "github.com" || host == "raw.githubusercontent.com") && len(parts) >= 2 {
		return "github.com/" + strings.ToLower(parts[0]+"/"+strings.TrimSuffix(parts[1], ".git"))
	}
	if raw == "" {
		return ""
	}
	return host + "/" + path
}
func entryRepositories(e extractedMod) map[string]bool {
	result := map[string]bool{}
	if key := repositoryKey(e.RepositoryURL); key != "" {
		result[key] = true
	}
	for _, d := range e.Downloads {
		u, _ := url.Parse(d.URL)
		if u != nil && (u.Hostname() == "github.com" || u.Hostname() == "raw.githubusercontent.com") {
			result[repositoryKey(d.URL)] = true
		}
	}
	return result
}

// Auto matching never assigns ownership from an anonymous display name. A name
// match alone requires operator resolution; repository+name or an explicit
// earlier announcement supplies the additional provenance.
func matchImport(tx *sql.Tx, c importCandidate, owner int64) (int64, error) {
	if c.Entry.Confidence != "clear" {
		return 0, problem(409, "Extraction needs review: "+c.Entry.Reason)
	}
	if strings.Contains(c.Entry.Name, "/") {
		return 0, problem(409, "Possible combined mod names; confirm listing identity.")
	}
	rows, err := tx.Query(modSelect+" ORDER BY m.id", 0)
	if err != nil {
		return 0, err
	}
	list := []mod{}
	for rows.Next() {
		m, e := scanMod(rows)
		if e != nil {
			rows.Close()
			return 0, e
		}
		list = append(list, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	// A quoted announcement can contain several mods. Its post ID alone does
	// not identify which of those releases is being updated, even if only one
	// entry has been published so far.
	var previousEntryCount int
	if c.Entry.PreviousPostID > 0 {
		if err = tx.QueryRow("SELECT COUNT(*) FROM ingest_candidates WHERE post_id=?", c.Entry.PreviousPostID).Scan(&previousEntryCount); err != nil {
			return 0, err
		}
	}
	predecessors := map[int64]bool{}
	// Accepted history remembers names that an operator has reconciled. An alias
	// can prevent a duplicate, but does not bypass provenance or manual-edit checks.
	aliases := map[int64]map[string]bool{}
	history, err := tx.Query("SELECT mod_id,post_id,payload FROM ingest_candidates WHERE state='published' AND mod_id IS NOT NULL")
	if err != nil {
		return 0, err
	}
	for history.Next() {
		var id, post int64
		var raw string
		var entry extractedMod
		if err = history.Scan(&id, &post, &raw); err != nil {
			history.Close()
			return 0, err
		}
		if err = json.Unmarshal([]byte(raw), &entry); err != nil {
			history.Close()
			return 0, err
		}
		if aliases[id] == nil {
			aliases[id] = map[string]bool{}
		}
		aliases[id][normalizedName(entry.Name)] = true
		if post == c.Entry.PreviousPostID {
			predecessors[id] = true
		}
	}
	err = history.Err()
	history.Close()
	if err != nil {
		return 0, err
	}
	possible := []mod{}
	previousMatches := map[int64]bool{}
	repos := entryRepositories(c.Entry)
	for _, m := range list {
		sameName := normalizedName(m.Name) == normalizedName(c.Entry.Name)
		linked := false
		for _, d := range m.Downloads {
			for _, next := range c.Entry.Downloads {
				if d.URL == next.URL {
					linked = true
				}
			}
		}
		sameRepo := false
		for key := range entryRepositories(extractedMod{RepositoryURL: m.RepositoryURL, Downloads: m.Downloads}) {
			if repos[key] {
				sameRepo = true
			}
		}
		possibleAlias := false
		if sameRepo {
			left, right := normalizedName(m.Name), normalizedName(c.Entry.Name)
			possibleAlias = strings.HasPrefix(left, right+" ") || strings.HasPrefix(right, left+" ")
			hasPackage := false
			for _, d := range c.Entry.Downloads {
				stem := packageStem(d.URL)
				if stem != "" {
					hasPackage = true
				}
				for _, old := range m.Downloads {
					if stem != "" && stem == packageStem(old.URL) {
						possibleAlias = true
					}
				}
			}
			// A repository/release landing page cannot distinguish another mod
			// in a monorepo from a renamed listing without operator input.
			if !hasPackage {
				possibleAlias = true
			}
		}
		historicalName := aliases[m.ID][normalizedName(c.Entry.Name)]
		previousMatch := predecessors[m.ID] && (previousEntryCount == 1 || sameName || historicalName || linked)
		previousMatches[m.ID] = previousMatch
		if sameName || historicalName || linked || possibleAlias || previousMatch {
			possible = append(possible, m)
		}
	}
	if len(possible) == 0 {
		if previousEntryCount > 1 {
			return 0, problem(409, "Earlier post contains multiple mods; confirm which listing this updates.")
		}
		return 0, nil
	}
	if len(possible) > 1 {
		return 0, problem(409, "Multiple existing listings may match.")
	}
	m := possible[0]
	if m.UserID != owner || m.SourceURL == "" {
		return 0, problem(409, "Existing listing requires an explicit merge.")
	}
	matched := previousMatches[m.ID]
	if normalizedName(m.Name) == normalizedName(c.Entry.Name) {
		for key := range entryRepositories(extractedMod{RepositoryURL: m.RepositoryURL, Downloads: m.Downloads}) {
			if repos[key] {
				matched = true
			}
		}
		if sameReleaseDownloads(m.Downloads, c.Entry.Downloads) {
			matched = true
		}
	}
	if !matched {
		return 0, problem(409, "Name matches, but update provenance is ambiguous.")
	}
	return m.ID, nil
}

func (a *app) publishCandidate(ctx context.Context, id, owner int64, actor *identity, target *int64, revision int64) (int64, error) {
	var tx *sql.Tx
	var err error
	if actor != nil {
		tx, err = a.beginWrite(*actor)
	} else {
		tx, err = a.db.BeginTx(ctx, nil)
	}
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var role string
	if err = tx.QueryRow("SELECT role FROM users WHERE id=?", owner).Scan(&role); err != nil {
		return 0, err
	}
	if role != "admin" {
		return 0, problem(403, "Importer owner must be an existing admin account.")
	}
	c, err := candidateFrom(tx.QueryRow(candidateSelect+" WHERE c.id=? AND c.state='pending'", id))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, problem(409, "Candidate already handled or removed.")
	}
	if err != nil {
		return 0, err
	}
	if excludedRelease(c.Entry) {
		return 0, problem(409, "Base game or index release is outside the mod catalog.")
	}
	in := c.Entry.input()
	in.ImageURL = "" // Images are fetched separately, never under the write transaction.
	if err = in.validate(0); err != nil {
		return 0, err
	}
	var modID int64
	if target == nil {
		modID, err = matchImport(tx, c, owner)
		if err != nil {
			return 0, err
		}
	} else {
		modID = *target
	}
	var old mod
	if modID > 0 {
		old, err = scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, modID))
		if err != nil {
			return 0, err
		}
		if target != nil && revision != old.Revision {
			return 0, problem(409, "Listing changed. Reload before merging.")
		}
		var priorTime int64
		var priorFingerprint string
		err = tx.QueryRow("SELECT posted_at,fingerprint FROM mod_sources WHERE mod_id=?", modID).Scan(&priorTime, &priorFingerprint)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if priorTime >= c.Source.Time {
			return 0, problem(409, "An equal or newer release post is already indexed. Use Add to history to keep this post without replacing the current release.")
		}
		if target == nil && (old.UserID != owner || priorFingerprint != fingerprint(old)) {
			return 0, problem(409, "Listing was edited manually; review this update.")
		}
		if in.Version == "Unversioned" && sameReleaseDownloads(old.Downloads, in.Downloads) {
			in.Version = old.Version
		}
		if target == nil && olderVersion(in.Version, old.Version) {
			return 0, problem(409, "Source appears to downgrade the version; review this update.")
		}
		in.ImageURL = old.ImageURL
		// Keep the established overview until the history-based summary is ready.
		// Per-release notes stay in the immutable published candidate payload.
		in.ShortDescription = old.ShortDescription
		in.Description = old.Description
	}
	downloads, _ := json.Marshal(in.Downloads)
	postedAt := c.Source.Time
	if modID == 0 {
		result, err := tx.Exec(`INSERT INTO mods(user_id,name,version,game_version,download_url,downloads_json,repository_url,short_description,description,image_url,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'',?,?)`, owner, in.Name, in.Version, in.GameVersion, in.DownloadURL, string(downloads), in.RepositoryURL, in.ShortDescription, in.Description, postedAt, postedAt)
		if err != nil {
			return 0, err
		}
		modID, err = result.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		bump := 0
		if old.Version != in.Version || !sameReleaseDownloads(old.Downloads, in.Downloads) {
			bump = 1
		}
		_, err = tx.Exec(`UPDATE mods SET name=?,version=?,game_version=?,download_url=?,downloads_json=?,repository_url=?,short_description=?,description=?,updated_at=?,revision=revision+? WHERE id=?`, in.Name, in.Version, in.GameVersion, in.DownloadURL, string(downloads), in.RepositoryURL, in.ShortDescription, in.Description, postedAt, bump, modID)
		if err != nil {
			return 0, err
		}
	}
	current, err := scanMod(tx.QueryRow(modSelect+" WHERE m.id=?", 0, modID))
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO mod_sources(mod_id,post_id,author,source_url,posted_at,fingerprint) VALUES(?,?,?,?,?,?) ON CONFLICT(mod_id) DO UPDATE SET post_id=excluded.post_id,author=excluded.author,source_url=excluded.source_url,posted_at=excluded.posted_at,fingerprint=excluded.fingerprint`, modID, c.PostID, c.Source.Author, c.Source.URL, c.Source.Time, fingerprint(current))
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec("UPDATE ingest_candidates SET state='published',mod_id=? WHERE id=?", modID, id)
	if err != nil {
		return 0, err
	}
	if err = refreshSourceDates(ctx, tx, modID); err != nil {
		return 0, err
	}
	return modID, tx.Commit()
}
func (a *app) listCandidates(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if i.User.Role != "admin" {
		return problem(403, "Admin access required.")
	}
	rows, err := a.db.Query(candidateSelect + " WHERE c.state='pending' ORDER BY c.post_id,c.ordinal LIMIT 500")
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []importCandidate{}
	for rows.Next() {
		c, err := candidateFrom(rows)
		if err != nil {
			return err
		}
		list = append(list, c)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for index := range list {
		list[index].Context, err = a.postContext([]threadPost{list[index].Source})
		if err != nil {
			return err
		}
	}
	return writeJSON(w, 200, list)
}
func (a *app) resolveCandidate(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if i.User.Role != "admin" {
		return problem(403, "Admin access required.")
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	var in struct {
		Action   string `json:"action"`
		TargetID int64  `json:"target_id"`
		Revision int64  `json:"revision"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Action == "reject" {
		tx, err := a.beginWrite(i)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.Exec("UPDATE ingest_candidates SET state='rejected' WHERE id=? AND state='pending'", id)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return problem(409, "Candidate already handled or removed.")
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]bool{"ok": true})
	}
	if (in.Action != "publish" && in.Action != "history") || in.TargetID < 0 || in.Action == "history" && in.TargetID == 0 {
		return problem(400, "Choose publish, history for an existing listing, or reject.")
	}
	c, err := candidateFrom(a.db.QueryRow(candidateSelect+" WHERE c.id=?", id))
	if err != nil {
		return err
	}
	if in.Action == "history" {
		if err = a.acceptCandidateHistory(r.Context(), id, in.TargetID, in.Revision, i); err != nil {
			return err
		}
		warning := ""
		if err = a.retainCandidateImage(r.Context(), c, in.TargetID, i.User.ID, &i, false); err != nil {
			warning = "History saved; source image could not be imported."
		}
		return writeJSON(w, 200, map[string]any{"id": in.TargetID, "warning": warning})
	}
	modID, err := a.publishCandidate(r.Context(), id, i.User.ID, &i, &in.TargetID, in.Revision)
	if err != nil {
		return err
	}
	warning := ""
	if err = a.importCandidateImage(r.Context(), c, modID, i.User.ID, &i); err != nil {
		warning = "Listing saved; image import failed. Use Edit to add an image."
	}
	return writeJSON(w, 200, map[string]any{"id": modID, "warning": warning})
}
func (a *app) publicSite(w http.ResponseWriter, r *http.Request) error {
	var thread string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='current_thread'").Scan(&thread); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"current_thread": "https://boards.4chan.org/mlp/thread/" + thread})
}

func (a *app) listImportErrors(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if i.User.Role != "admin" {
		return problem(403, "Admin access required.")
	}
	rows, err := a.db.Query("SELECT id,reason,payload FROM ingest_posts WHERE state='candidate' AND reason LIKE 'Extraction needs retry:%' ORDER BY id LIMIT 500")
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id int64
		var reason, b string
		if err = rows.Scan(&id, &reason, &b); err != nil {
			return err
		}
		var p threadPost
		if err = json.Unmarshal([]byte(b), &p); err != nil {
			return err
		}
		list = append(list, map[string]any{"id": id, "reason": reason, "source": p})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}
func (a *app) retryImportPost(w http.ResponseWriter, r *http.Request) error {
	i, err := a.require(r, true)
	if err != nil {
		return err
	}
	if i.User.Role != "admin" {
		return problem(403, "Admin access required.")
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	var in struct {
		Action string `json:"action"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Action != "retry" && in.Action != "dismiss" {
		return problem(400, "Use retry or dismiss.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := "candidate"
	if in.Action == "dismiss" {
		state = "other"
	}
	result, err := tx.Exec("UPDATE ingest_posts SET state=?,reason='' WHERE id=? AND state='candidate' AND reason LIKE 'Extraction needs retry:%'", state, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return problem(409, "Post already handled or removed.")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]bool{"ok": true})
}

var plainReleaseVersion = regexp.MustCompile(`^[vV]?[0-9]+(?:\.[0-9]+){1,3}$`)

func olderVersion(next, previous string) bool {
	if !plainReleaseVersion.MatchString(next) || !plainReleaseVersion.MatchString(previous) {
		return false
	}
	a := strings.Split(strings.TrimLeft(next, "vV"), ".")
	b := strings.Split(strings.TrimLeft(previous, "vV"), ".")
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y uint64
		var err error
		if i < len(a) {
			x, err = strconv.ParseUint(a[i], 10, 64)
			if err != nil {
				return false
			}
		}
		if i < len(b) {
			y, err = strconv.ParseUint(b[i], 10, 64)
			if err != nil {
				return false
			}
		}
		if x != y {
			return x < y
		}
	}
	return false
}

var packageVersionSuffix = regexp.MustCompile(`(?i)[-_ .]?v?[0-9]+(?:[._][0-9]+)+(?:[-_][a-z0-9]+)?$`)

func packageStem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	name := downloadFilename(u.String())
	if !strings.HasSuffix(strings.ToLower(name), ".pck") {
		return ""
	}
	return normalizedName(packageVersionSuffix.ReplaceAllString(strings.TrimSuffix(name, ".pck"), ""))
}
