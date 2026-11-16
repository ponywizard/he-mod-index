package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultipartDownloadsAndApproval(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.register(t, "owner")
	in := sampleMod("Breeze TTS")
	in.Downloads = []download{{"breeze.pck", "https://example.org/breeze.pck"}, {"server-linux.tar.gz", "https://example.org/linux.tar.gz"}, {"server-windows.zip", "https://example.org/windows.zip"}, {"voices.zip", "https://example.org/voices.zip"}}
	w := owner.request("POST", "/api/mods", in, nil)
	status(t, w, 201)
	m := readJSON[mod](t, w)
	if len(m.Downloads) != 4 || m.Downloads[2].Filename != "server-windows.zip" || m.DownloadURL != in.Downloads[0].URL {
		t.Fatalf("downloads did not round trip: %+v", m.Downloads)
	}
	reviewer := client(a, "192.0.2.2")
	reviewer.provision(t, "devanon", "reviewer")
	approve := func() {
		status(t, reviewer.request("PUT", modPath(m)+"/approval", map[string]any{"approved": true, "revision": m.Revision}, nil), 200)
	}
	approve()
	in = m.modInput
	in.Downloads = append([]download(nil), m.Downloads...)
	in.Downloads[0].Filename = "renamed-mod.pck"
	in.Downloads[0], in.Downloads[1] = in.Downloads[1], in.Downloads[0]
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Revision != 1 || m.Approval != "current" {
		t.Fatal("renaming/reordering files changed release")
	}
	in.Downloads[2].URL = "https://example.org/windows-v2.zip"
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Revision != 2 || m.Approval != "stale" {
		t.Fatal("changing secondary download did not fade check")
	}
	approve()
	in.Downloads = in.Downloads[:3]
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Revision != 3 || m.Approval != "stale" {
		t.Fatal("removing file did not fade check")
	}
	approve()
	in.Downloads = append(in.Downloads, download{"new-voices.zip", "https://example.org/new-voices.zip"})
	w = owner.request("PUT", modPath(m), in, nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.Revision != 4 || m.Approval != "stale" {
		t.Fatal("adding file did not fade check")
	}
	status(t, owner.request("PUT", modPath(m), sampleMod("Legacy cannot erase multiple downloads"), nil), 400)
	legacy := owner.addMod(t, "Legacy API")
	if len(legacy.Downloads) != 1 || legacy.Downloads[0].Filename != "mod-1.pck" {
		t.Fatal("legacy API missing inferred filename")
	}
	for _, downloads := range [][]download{{}, {{"bad/name.pck", "https://example.org/a"}}, {{"bad\nname.pck", "https://example.org/a"}}, {{"file.pck", "javascript:alert(1)"}}, {{"", "https://example.org/a"}}, {{"a", "https://example.org/a"}, {"b", "https://example.org/a"}}} {
		bad := sampleMod("Bad")
		bad.Downloads = downloads
		status(t, owner.request("POST", "/api/mods", bad, nil), 400)
	}
}

func splitImage(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 600, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 600; x++ {
			if x < 300 {
				im.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
			} else {
				im.SetRGBA(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func checkJPEG(t *testing.T, data []byte, w, h int, want string) {
	t.Helper()
	im, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds().Dx() != w || im.Bounds().Dy() != h {
		t.Fatalf("image dimensions: %v", im.Bounds())
	}
	r, _, b, _ := im.At(w/2, h/2).RGBA()
	if want == "blue" && (b < 60000 || r > 1500) {
		t.Fatalf("expected blue crop, got r=%d b=%d", r, b)
	}
	if want == "red" && (r < 60000 || b > 1500) {
		t.Fatalf("expected red crop, got r=%d b=%d", r, b)
	}
}
func TestThumbnailCropAndLifecycle(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	u := owner.register(t, "owner")
	m := owner.addMod(t, "Crop")
	w := owner.request("POST", modPath(m)+"/image?x=300&y=0&size=300", splitImage(t), nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	if m.ImageCrop != (cropRect{300, 0, 300}) || m.ThumbnailURL != thumbnailURL(m.ImageURL) {
		t.Fatalf("missing stored crop: %+v", m)
	}
	thumb := owner.request("GET", m.ThumbnailURL, nil, nil)
	status(t, thumb, 200)
	checkJPEG(t, thumb.Body.Bytes(), 256, 256, "blue")
	full := owner.request("GET", m.ImageURL, nil, nil)
	status(t, full, 200)
	checkJPEG(t, full.Body.Bytes(), 600, 300, "")
	for _, crop := range []cropRect{{-1, 0, 300}, {0, 0, 0}, {301, 0, 300}, {0, 1, 300}, {0, 0, 2147483647}} {
		status(t, owner.request("POST", modPath(m)+"/image/crop", crop, nil), 400)
	}
	status(t, owner.request("POST", modPath(m)+"/image?x=0", splitImage(t), nil), 400)
	status(t, owner.request("POST", modPath(m)+"/image?x=0&y=0&size=oops", splitImage(t), nil), 400)
	w = owner.request("POST", modPath(m)+"/image/crop", cropRect{0, 0, 300}, nil)
	status(t, w, 200)
	next := readJSON[mod](t, w)
	checkJPEG(t, owner.request("GET", next.ThumbnailURL, nil, nil).Body.Bytes(), 256, 256, "red")
	if !bytes.Equal(full.Body.Bytes(), owner.request("GET", next.ImageURL, nil, nil).Body.Bytes()) {
		t.Fatal("recropping recompressed full screenshot")
	}
	status(t, owner.request("GET", m.ImageURL, nil, nil), 404)
	status(t, owner.request("GET", m.ThumbnailURL, nil, nil), 404)
	other := client(a, "192.0.2.2")
	other.register(t, "other")
	status(t, other.request("POST", modPath(next)+"/image/crop", cropRect{0, 0, 100}, nil), 403)
	admin := client(a, "192.0.2.3")
	admin.provision(t, "ponywizard", "admin")
	status(t, admin.request("DELETE", fmt.Sprintf("/api/admin/users/%d", u.ID), nil, nil), 200)
	status(t, owner.request("GET", next.ImageURL, nil, nil), 404)
	status(t, owner.request("GET", next.ThumbnailURL, nil, nil), 404)
	files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
	if err != nil || len(files) != 0 {
		t.Fatal("image files leaked after deletion", files, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fakeImageResponse(r *http.Request, status int, data []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}
}
func TestRemoteImagesAreImportedAndValidated(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.register(t, "owner")
	source := splitImage(t)
	calls := 0
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials forwarded to remote image server")
		}
		return fakeImageResponse(r, 200, source), nil
	})
	in := sampleMod("Imported")
	in.ImageURL = "https://images.example/screenshot.png"
	w := owner.request("POST", "/api/mods", in, nil)
	status(t, w, 201)
	m := readJSON[mod](t, w)
	if !validLocalImage(m.ImageURL, m.ID) || m.ThumbnailURL == "" || calls != 1 {
		t.Fatalf("remote image was not stored locally: %+v, calls=%d", m, calls)
	}
	status(t, owner.request("GET", m.ImageURL, nil, nil), 200)
	w = owner.request("POST", "/api/images/preview", map[string]string{"url": "https://images.example/preview.png"}, nil)
	status(t, w, 200)
	checkJPEG(t, w.Body.Bytes(), 600, 300, "")
	before := calls
	status(t, owner.request("POST", "/api/images/preview", map[string]string{"url": "http://127.0.0.1/private"}, nil), 400)
	if calls != before {
		t.Fatal("attempted fetch of private target")
	}
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fakeImageResponse(r, 200, []byte("<html>not an image</html>")), nil
	})
	bad := m.modInput
	bad.ImageURL = "https://images.example/bad"
	status(t, owner.request("PUT", modPath(m), bad, nil), 400)
	current := readJSON[mod](t, owner.request("GET", modPath(m), nil, nil))
	if current.ImageURL != m.ImageURL {
		t.Fatal("failed import replaced old image")
	}
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fakeImageResponse(r, 200, make([]byte, maxImageBytes+1)), nil
	})
	status(t, owner.request("POST", "/api/images/preview", map[string]string{"url": "https://images.example/large"}, nil), 413)
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		res := fakeImageResponse(r, 302, nil)
		res.Header.Set("Location", "http://169.254.169.254/latest/meta-data/")
		return res, nil
	})
	status(t, owner.request("POST", "/api/images/preview", map[string]string{"url": "https://images.example/redirect"}, nil), 400)
	bad = m.modInput
	bad.ImageURL = ""
	w = owner.request("PUT", modPath(m), bad, nil)
	status(t, w, 200)
	cleared := readJSON[mod](t, w)
	if cleared.ImageURL != "" || cleared.ThumbnailURL != "" || cleared.ImageCrop.Size != 0 {
		t.Fatal("removing image retained thumbnail")
	}
	status(t, owner.request("GET", m.ThumbnailURL, nil, nil), 404)
}

func TestRemoteImageDestinationChecks(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.1.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1", "198.18.0.1", "192.0.2.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1"} {
		if publicImageIP(netip.MustParseAddr(ip)) {
			t.Errorf("allowed private/special target %s", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicImageIP(netip.MustParseAddr(ip)) {
			t.Errorf("blocked public target %s", ip)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://user:password@images.example/x", "http://images.example:8080/x", "https://images.example:444/x", "http://127.0.0.1/x", "http://[::1]/x"} {
		if _, err := imageURL(raw); err == nil {
			t.Errorf("accepted image URL %s", raw)
		}
	}
	resolved, dialed := 0, []string{}
	transport := imageTransport(func(ctx context.Context, host string) ([]netip.Addr, error) {
		resolved++
		if resolved == 1 {
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, errors.New("test dial")
	})
	transport.DialContext(context.Background(), "tcp", "images.example:443")
	transport.DialContext(context.Background(), "tcp", "images.example:443")
	if resolved != 2 || len(dialed) != 1 || dialed[0] != "1.1.1.1:443" {
		t.Fatalf("DNS destination was not pinned/checked: resolved=%d dialed=%v", resolved, dialed)
	}
	transport = imageTransport(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.0.0.1")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed mixed public/private DNS result")
		return nil, errors.New("unexpected")
	})
	if _, err := transport.DialContext(context.Background(), "tcp", "images.example:443"); err == nil {
		t.Fatal("accepted mixed DNS result")
	}
}

func TestImageImportRejectsConcurrentReplacement(t *testing.T) {
	a := testApp(t)
	owner := client(a, "192.0.2.1")
	owner.register(t, "owner")
	m := owner.addMod(t, "Concurrent images")
	w := owner.request("POST", modPath(m)+"/image", splitImage(t), nil)
	status(t, w, 200)
	m = readJSON[mod](t, w)
	started, proceed := make(chan struct{}), make(chan struct{})
	data := splitImage(t)
	a.imageClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-proceed
		return fakeImageResponse(r, 200, data), nil
	})
	in := m.modInput
	in.ImageURL = "https://images.example/new.png"
	done := make(chan *httptest.ResponseRecorder, 1)
	writer := *owner
	go func() { done <- writer.request("PUT", modPath(m), in, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("image import did not start")
	}
	w = owner.request("POST", modPath(m)+"/image/crop", cropRect{0, 0, 300}, nil)
	close(proceed)
	status(t, w, 200)
	cropped := readJSON[mod](t, w)
	select {
	case result := <-done:
		status(t, result, 409)
	case <-time.After(5 * time.Second):
		t.Fatal("image import did not finish")
	}
	current := readJSON[mod](t, owner.request("GET", modPath(m), nil, nil))
	if current.ImageURL != cropped.ImageURL || current.ThumbnailURL != cropped.ThumbnailURL {
		t.Fatal("concurrent metadata save overwrote newer image")
	}
	files, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "uploads"))
	if err != nil || len(files) != 2 {
		t.Fatal("staged image files leaked after conflict", err, len(files))
	}
}

func TestVersionOneMigrationPreservesListingsAndBuildsThumbnails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	url := "/uploads/1-" + randomToken() + ".jpg"
	if _, err = db.Exec(`INSERT INTO users(id,username,password,role,created_at) VALUES(1,'author','test','reviewer',1);
	INSERT INTO mods(id,user_id,name,version,download_url,short_description,image_url,created_at,updated_at,revision,approved_revision,approved_by,approved_at) VALUES(1,1,'Legacy','1.0','https://example.org/releases/mod%20one.pck?raw=1','Legacy mod',?,10,20,4,4,1,30)`, url); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.MkdirAll(filepath.Join(dir, "uploads"), 0700)
	im, err := decodeImage(splitImage(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeImage(im)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, strings.TrimPrefix(url, "/")), data, 0600)
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := newApp(db, config{DataDir: dir, PublicURL: "http://localhost:8080", RegistrationCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.readMod(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Downloads) != 1 || m.Downloads[0].Filename != "mod one.pck" || m.Downloads[0].URL != m.DownloadURL || m.Revision != 4 || m.Approval != "current" || m.CreatedAt != 10 || m.UpdatedAt != 20 || m.ThumbnailURL == "" {
		t.Fatalf("migration lost listing data: %+v", m)
	}
	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 3 {
		t.Fatalf("schema version=%d", version)
	}
	if err = migrateDB(db); err != nil {
		t.Fatal("migration is not repeatable", err)
	}
	if _, err = newApp(db, a.cfg); err != nil {
		t.Fatal(err)
	}
	next, _ := a.readMod(1, 0)
	if next.ImageURL != m.ImageURL {
		t.Fatal("startup regenerated thumbnail twice")
	}
	db.Exec("PRAGMA user_version=99")
	if err = migrateDB(db); err == nil {
		t.Fatal("accepted newer database schema")
	}
}
