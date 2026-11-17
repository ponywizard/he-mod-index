package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func attachCandidate(t *testing.T, a *app, c importCandidate, url string) importCandidate {
	t.Helper()
	c.Source.Image = url
	raw, _ := json.Marshal(c.Source)
	if _, err := a.db.Exec("UPDATE ingest_posts SET payload=? WHERE id=?", string(raw), c.PostID); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAttachedImageWinsWithoutChangingPublishedExtraction(t *testing.T) {
	a, admin, _, calls := galleryFixture(t)
	e := exampleExtraction()
	e.ImageURL = "" // The old model skipped the reaction picture.
	c := attachCandidate(t, a, seedCandidate(t, a, 40000001, e), "https://example.org/reaction.jpg")
	status(t, admin.request("POST", fmt.Sprintf("/api/admin/imports/%d", c.ID), map[string]any{"action": "publish"}, nil), 200)
	var id int64
	var payload string
	a.db.QueryRow("SELECT mod_id,payload FROM ingest_candidates WHERE id=?", c.ID).Scan(&id, &payload)
	m, err := a.readMod(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.ImageURL == "" || *calls != 1 {
		t.Fatal("attachment omitted", m, *calls)
	}
	assertImagePair(t, a, m.ImageURL, true)
	var accepted extractedMod
	json.Unmarshal([]byte(payload), &accepted)
	if accepted.ImageURL != "" {
		t.Fatal("rewrote immutable extraction", accepted)
	}
	gallery := readJSON[[]modImage](t, admin.request("GET", modPath(m)+"/images", nil, nil))
	if len(gallery) != 1 || gallery[0].PostID != c.PostID || gallery[0].Version != e.Version {
		t.Fatal(gallery)
	}
}

func TestAttachmentBackfillAndMissingCoverProtectEditsAndRemoval(t *testing.T) {
	for _, kind := range []string{"missing", "existing gallery", "manual edit", "removed"} {
		t.Run(kind, func(t *testing.T) {
			a, admin, owner, calls := galleryFixture(t)
			c := attachCandidate(t, a, seedCandidate(t, a, 40000001, exampleExtraction()), "https://example.org/reaction.jpg")
			id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "existing gallery" || kind == "removed" {
				if err = a.backfillGallery(context.Background(), owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			m, _ := a.readMod(id, 0)
			if kind == "manual edit" {
				m.Description = "Owner correction"
				status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
			}
			if kind == "removed" {
				a.db.Exec("UPDATE mod_images SET image_url='',thumbnail_url='' WHERE candidate_id=?", c.ID)
			}
			before, _ := a.readMod(id, 0)
			if err = a.backfillGallerySelection(context.Background(), owner.ID, id, true); err != nil {
				t.Fatal(err)
			}
			after, _ := a.readMod(id, 0)
			if kind == "manual edit" || kind == "removed" {
				if fingerprint(before) != fingerprint(after) {
					t.Fatal("overwrote an edit or removal")
				}
			} else if after.ImageURL == "" {
				t.Fatal("cover still missing")
			}
			if before.Revision != after.Revision || before.CreatedAt != after.CreatedAt || before.UpdatedAt != after.UpdatedAt {
				t.Fatal("image changed release or dates")
			}
			fetched := *calls
			if err = a.backfillGallerySelection(context.Background(), owner.ID, id, true); err != nil {
				t.Fatal(err)
			}
			if *calls != fetched {
				t.Fatal("backfill refetched an existing image")
			}
		})
	}
}

func TestAttachmentImportRejectsSourceChangeDuringFetch(t *testing.T) {
	a, _, owner, _ := galleryFixture(t)
	c := attachCandidate(t, a, seedCandidate(t, a, 40000001, exampleExtraction()), "https://example.org/reaction.jpg")
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	original := a.imageClient.Transport
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		changed := c.Source
		changed.Image = "https://example.org/other.jpg"
		raw, _ := json.Marshal(changed)
		if _, err := a.db.Exec("UPDATE ingest_posts SET payload=? WHERE id=?", string(raw), c.PostID); err != nil {
			t.Fatal(err)
		}
		return original.RoundTrip(r)
	})
	if err = a.importCandidateImage(context.Background(), c, id, owner.ID, nil); err == nil {
		t.Fatal("source change was ignored")
	}
	m, _ := a.readMod(id, 0)
	if m.ImageURL != "" {
		t.Fatal("changed source attached")
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM mod_images").Scan(&count)
	if count != 0 {
		t.Fatal("changed source persisted")
	}
}
