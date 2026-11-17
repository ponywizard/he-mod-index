package main

import (
	"net/http"
)

func (a *app) admin(r *http.Request) (identity, error) {
	i, err := a.require(r, true)
	if err != nil {
		return i, err
	}
	if i.User.Role != "admin" {
		return i, problem(403, "Admin access required.")
	}
	return i, nil
}
func (a *app) listUsers(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.admin(r); err != nil {
		return err
	}
	rows, err := a.db.Query(`SELECT u.id,u.username,u.role,u.created_at,(SELECT COUNT(*) FROM mods WHERE user_id=u.id),(SELECT COUNT(*) FROM votes WHERE user_id=u.id) FROM users u ORDER BY u.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var name, role string
		var mods, votes int
		if err = rows.Scan(&id, &name, &role, &created, &mods, &votes); err != nil {
			return err
		}
		list = append(list, map[string]any{"id": id, "username": name, "role": role, "created_at": created, "mods": mods, "votes": votes})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}
func (a *app) deleteUser(w http.ResponseWriter, r *http.Request) error {
	i, err := a.admin(r)
	if err != nil {
		return err
	}
	id, err := idParam(r)
	if err != nil {
		return err
	}
	if id == i.User.ID {
		return problem(400, "You cannot remove your own admin account.")
	}
	tx, err := a.beginWrite(i)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id,image_url FROM mods WHERE user_id=? UNION SELECT g.mod_id,g.image_url FROM mod_images g JOIN mods m ON m.id=g.mod_id WHERE m.user_id=?", id, id)
	if err != nil {
		return err
	}
	type image struct {
		id  int64
		url string
	}
	images := []image{}
	for rows.Next() {
		var im image
		if err = rows.Scan(&im.id, &im.url); err != nil {
			rows.Close()
			return err
		}
		images = append(images, im)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM users WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return problem(404, "User not found.")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, im := range images {
		a.removeImage(im.url, im.id)
	}
	return writeJSON(w, 200, map[string]bool{"ok": true})
}
