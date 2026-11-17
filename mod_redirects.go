package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Merged listing URLs remain readable. Writes to retired IDs are deliberately
// not redirected, so a stale editor cannot modify a different listing.
func (a *app) redirectMergedMod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "GET" && r.Method != "HEAD" {
		return false, nil
	}
	prefix := "/mod/"
	if strings.HasPrefix(r.URL.Path, "/api/mods/") {
		prefix = "/api/mods/"
	}
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id < 1 {
		return false, nil
	}
	var target int64
	err = a.db.QueryRow("SELECT mod_id FROM mod_redirects WHERE old_id=?", id).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parts[0] = strconv.FormatInt(target, 10)
	location := prefix + strings.Join(parts, "/")
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, location, http.StatusPermanentRedirect)
	return true, nil
}
