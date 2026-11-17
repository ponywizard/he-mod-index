package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func galleryFixture(t *testing.T) (*app, *testClient, user, *int) {
	t.Helper()
	a := testApp(t)
	admin := client(a, "192.0.2.1")
	owner := admin.provision(t, "private_operator", "admin")
	calls := new(int)
	a.imageClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if strings.Contains(r.URL.Path, "missing") {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("missing")), Header: make(http.Header)}, nil
		}
		im := image.NewRGBA(image.Rect(0, 0, 16, 16))
		shade := color.RGBA{R: 220, A: 255}
		if strings.Contains(r.URL.Path, "second") {
			shade = color.RGBA{B: 220, A: 255}
		}
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				im.Set(x, y, shade)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Error(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b.Bytes())), Header: make(http.Header)}, nil
	})}
	return a, admin, owner, calls
}
func publishImageFixture(t *testing.T, a *app, owner user, post int64, version, filename string) (importCandidate, mod) {
	t.Helper()
	e := exampleExtraction()
	e.Version = version
	e.ImageURL = "https://example.org/" + filename
	c := seedCandidate(t, a, post, e)
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.importCandidateImage(context.Background(), c, id, owner.ID, nil); err != nil {
		t.Fatal(err)
	}
	m, err := a.readMod(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}
func assertImagePair(t *testing.T, a *app, url string, want bool) {
	t.Helper()
	for _, path := range []string{url, thumbnailURL(url)} {
		_, err := os.Stat(filepath.Join(a.cfg.DataDir, strings.TrimPrefix(path, "/")))
		if want && err != nil || !want && !os.IsNotExist(err) {
			t.Fatalf("image %s presence expected %t: %v", path, want, err)
		}
	}
}
func TestGalleryRetainsReleasesAndRespectsImageEdits(t *testing.T) {
	a, admin, owner, calls := galleryFixture(t)
	first, m1 := publishImageFixture(t, a, owner, 40000001, "1.0", "first.png")
	second, m2 := publishImageFixture(t, a, owner, 40000002, "2.0", "second.png")
	assertImagePair(t, a, m1.ImageURL, true)
	assertImagePair(t, a, m2.ImageURL, true)
	held := exampleExtraction()
	held.ImageURL = "https://example.org/held.png"
	seedCandidate(t, a, 40000003, held)
	anon := client(a, "192.0.2.5")
	w := anon.request("GET", modPath(m2)+"/images", nil, nil)
	status(t, w, 200)
	pictures := readJSON[[]modImage](t, w)
	if len(pictures) != 2 || pictures[0].Version != "2.0" || pictures[1].PostID != first.PostID || pictures[0].SourceURL != "https://desuarchive.org/mlp/thread/40000000/#40000002" || strings.Contains(w.Body.String(), "private_operator") {
		t.Fatal(w.Body.String())
	}
	status(t, anon.request("GET", "/api/mods/999/images", nil, nil), 404)
	if err := a.importCandidateImage(context.Background(), second, m2.ID, owner.ID, nil); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatal("repeat import fetched again", *calls)
	}
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "reviewer", "reviewer")
	status(t, reviewer.request("PUT", modPath(m2)+"/approval", map[string]any{"approved": true, "revision": m2.Revision}, nil), 200)
	response := admin.request("POST", modPath(m2)+"/image/crop", cropRect{X: 0, Y: 0, Size: 8}, nil)
	status(t, response, 200)
	cropped := readJSON[mod](t, response)
	assertImagePair(t, a, m2.ImageURL, false)
	assertImagePair(t, a, cropped.ImageURL, true)
	pictures = readJSON[[]modImage](t, anon.request("GET", modPath(m2)+"/images", nil, nil))
	if pictures[0].ImageURL != cropped.ImageURL || pictures[0].Version != "2.0" || cropped.CreatedAt != m2.CreatedAt || cropped.UpdatedAt != m2.UpdatedAt || cropped.Approval != "current" {
		t.Fatal("crop lost provenance or release", pictures, cropped)
	}
	cropped.ImageURL = ""
	status(t, admin.request("PUT", modPath(m2), cropped.modInput, nil), 200)
	assertImagePair(t, a, pictures[0].ImageURL, false)
	if err := a.backfillGallery(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	pictures = readJSON[[]modImage](t, anon.request("GET", modPath(m2)+"/images", nil, nil))
	if len(pictures) != 1 || pictures[0].PostID != first.PostID || *calls != 2 {
		t.Fatal("removed image resurrected", pictures, *calls)
	}
	status(t, admin.request("DELETE", modPath(m2), nil, nil), 200)
	assertImagePair(t, a, m1.ImageURL, false)
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM mod_images").Scan(&count); err != nil || count != 0 {
		t.Fatal("gallery did not cascade", count, err)
	}
}
func TestGalleryFilesRemovedWithModOrUser(t *testing.T) {
	for _, removeUser := range []bool{false, true} {
		t.Run(map[bool]string{false: "mod", true: "user"}[removeUser], func(t *testing.T) {
			a, admin, owner, _ := galleryFixture(t)
			_, first := publishImageFixture(t, a, owner, 40000001, "1.0", "first.png")
			_, second := publishImageFixture(t, a, owner, 40000002, "2.0", "second.png")
			if removeUser {
				other := client(a, "192.0.2.2")
				other.provision(t, "other_admin", "admin")
				status(t, other.request("DELETE", fmt.Sprintf("/api/admin/users/%d", owner.ID), nil, nil), 200)
			} else {
				status(t, admin.request("DELETE", modPath(second), nil, nil), 200)
			}
			assertImagePair(t, a, first.ImageURL, false)
			assertImagePair(t, a, second.ImageURL, false)
			files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
			if err != nil || len(files) != 0 {
				t.Fatal("left gallery files", files, err)
			}
		})
	}
}
func TestGalleryMigrationAndBackfillKeepProvenance(t *testing.T) {
	a, _, owner, calls := galleryFixture(t)
	first, before := publishImageFixture(t, a, owner, 40000001, "1.0", "first.png")
	e := exampleExtraction()
	e.Version = "2.0"
	e.ImageURL = "https://example.org/missing.png"
	failed := seedCandidate(t, a, 40000002, e)
	if _, err := a.publishCandidate(context.Background(), failed.ID, owner.ID, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	before, _ = a.readMod(before.ID, 0)
	// Old installations only retain the last successfully downloaded cover.
	if _, err := a.db.Exec("DROP TABLE mod_images; PRAGMA user_version=7;"); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if err := a.backfillGallery(context.Background(), owner.ID); err == nil {
		t.Fatal("missing image was not reported")
	}
	got, _ := a.readMod(before.ID, 0)
	if !reflect.DeepEqual(before, got) {
		t.Fatal("backfill changed listing", before, got)
	}
	w := client(a, "192.0.2.5").request("GET", modPath(got)+"/images", nil, nil)
	status(t, w, 200)
	pictures := readJSON[[]modImage](t, w)
	if len(pictures) != 1 || pictures[0].PostID != first.PostID || pictures[0].Version != "1.0" || pictures[0].ImageURL != before.ImageURL {
		t.Fatal("old cover received incorrect version or was duplicated", pictures)
	}
	assertImagePair(t, a, before.ImageURL, true)
	files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
	if err != nil || len(files) != 2 {
		t.Fatal("redundant files from backfill", files, err)
	}
	if err = migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if err = a.backfillGallery(context.Background(), owner.ID); err == nil {
		t.Fatal("missing image was not retried")
	}
	if *calls != 4 {
		t.Fatal("successful backfill was not idempotent", *calls)
	}
}

func TestGalleryIgnoresVideosAndUnprovenUploads(t *testing.T) {
	a, admin, owner, calls := galleryFixture(t)
	e := exampleExtraction()
	e.ImageURL = "https://example.org/demo.WEBM?download=1"
	c := seedCandidate(t, a, 40000001, e)
	id, err := a.publishCandidate(context.Background(), c.ID, owner.ID, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.importCandidateImage(context.Background(), c, id, owner.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.backfillGallery(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatal("downloaded a video as an image")
	}
	m, _ := a.readMod(id, 0)
	m.ImageURL = "https://example.org/manual.png"
	status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
	result := client(a, "192.0.2.5").request("GET", modPath(m)+"/images", nil, nil)
	status(t, result, 200)
	if images := readJSON[[]modImage](t, result); len(images) != 0 {
		t.Fatal("manual upload received thread provenance", images)
	}
}

func TestGalleryBackfillPreservesLegacyImageRemoval(t *testing.T) {
	a, admin, owner, calls := galleryFixture(t)
	_, m := publishImageFixture(t, a, owner, 40000001, "1.0", "first.png")
	m.ImageURL = ""
	status(t, admin.request("PUT", modPath(m), m.modInput, nil), 200)
	if _, err := a.db.Exec("DROP TABLE mod_images; PRAGMA user_version=7;"); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(a.db); err != nil {
		t.Fatal(err)
	}
	if err := a.backfillGallery(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatal("retrieved a manually removed image")
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM mod_images").Scan(&count); err != nil || count != 0 {
		t.Fatal("resurrected a legacy image", count, err)
	}
}
