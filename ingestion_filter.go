package main

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// Quoted links are context, not a new distribution from the current post.
func authoredPostLinks(p threadPost) map[string]bool {
	lines := []string{}
	for _, line := range strings.Split(p.Text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") && !strings.HasPrefix(trimmed, ">>") {
			continue
		}
		lines = append(lines, line)
	}
	authored := normalizePost(threadPost{Text: strings.Join(lines, "\n")})
	links := map[string]bool{}
	for _, link := range authored.Links {
		links[link] = true
	}
	return links
}

func directDownloads(e extractedMod, p threadPost) []download {
	links := authoredPostLinks(p)
	downloads := []download{}
	for _, d := range e.Downloads {
		if links[d.URL] && distributionLink(d.URL) {
			downloads = append(downloads, d)
		}
	}
	return downloads
}

// Demonstration media are useful evidence, but are not installable downloads.
func distributionLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch host {
	case "youtube.com", "youtu.be", "odysee.com":
		return false
	}
	switch strings.ToLower(filepath.Ext(u.Path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".webm", ".mp4", ".mov":
		return false
	}
	return true
}

type importDismissal struct {
	ID     int64  `json:"id"`
	PostID int64  `json:"post_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Dismiss only still-pending entries. Published history and operator dismissals
// are never rewritten. First announcements with old URLs remain valid history.
func (a *app) prunePendingImports(ctx context.Context, dryRun bool) ([]importDismissal, error) {
	rows, err := a.db.QueryContext(ctx, candidateSelect+" WHERE c.state IN ('pending','published') ORDER BY p.posted_at,c.post_id,c.ordinal")
	if err != nil {
		return nil, err
	}
	list := []importCandidate{}
	for rows.Next() {
		c, err := candidateFrom(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := []importDismissal{}
	prior := []importCandidate{}
	for _, c := range list {
		var state, payload string
		if err = tx.QueryRowContext(ctx, "SELECT state,payload FROM ingest_candidates WHERE id=?", c.ID).Scan(&state, &payload); err != nil {
			return nil, err
		}
		// Preserve a concurrent operator decision; compare the payload before writing.
		var current extractedMod
		if err = json.Unmarshal([]byte(payload), &current); err != nil {
			return nil, err
		}
		c.Entry = current
		direct := directDownloads(c.Entry, c.Source)
		reason := ""
		if len(direct) == 0 {
			reason = "No download link in this post; links only refer to earlier announcements."
		} else {
			for _, old := range prior {
				if old.Source.Time >= c.Source.Time || normalizedName(old.Entry.Name) != normalizedName(c.Entry.Name) {
					continue
				}
				if c.Entry.Version != "Unversioned" && !strings.EqualFold(strings.TrimPrefix(c.Entry.Version, "v"), strings.TrimPrefix(old.Entry.Version, "v")) {
					continue
				}
				known := map[string]bool{}
				for _, d := range directDownloads(old.Entry, old.Source) {
					known[d.URL] = true
				}
				repeated := true
				for _, d := range direct {
					if !known[d.URL] {
						repeated = false
						break
					}
				}
				if repeated {
					reason = "Download links already appeared in an earlier announcement; no new version is stated."
					break
				}
			}
		}
		if state == "pending" && reason != "" {
			out = append(out, importDismissal{c.ID, c.PostID, c.Entry.Name, reason})
			if !dryRun {
				if _, err = tx.ExecContext(ctx, "UPDATE ingest_candidates SET state='rejected',reason=? WHERE id=? AND state='pending' AND payload=?", "Automatically dismissed: "+reason, c.ID, payload); err != nil {
					return nil, err
				}
			}
		} else if state == "published" || state == "pending" && reason == "" {
			prior = append(prior, c)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
