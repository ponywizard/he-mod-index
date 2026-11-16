package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxImageBytes = 4 << 20
const thumbnailSize = 256

var imageNamePattern = regexp.MustCompile(`^[1-9][0-9]*-[A-Z2-7]{26}(-thumb)?\.jpg$`)

type cropRect struct {
	X    int `json:"x"`
	Y    int `json:"y"`
	Size int `json:"size"`
}
type storedImage struct {
	ImageURL, ThumbnailURL string
	Crop                   cropRect
}

func validLocalImage(path string, id int64) bool {
	return strings.HasPrefix(path, fmt.Sprintf("/uploads/%d-", id)) && !strings.HasSuffix(path, "-thumb.jpg") && imageNamePattern.MatchString(strings.TrimPrefix(path, "/uploads/"))
}
func thumbnailURL(path string) string { return strings.TrimSuffix(path, ".jpg") + "-thumb.jpg" }
func (a *app) removeImage(path string, id int64) {
	if validLocalImage(path, id) {
		for _, p := range []string{path, thumbnailURL(path)} {
			_ = os.Remove(filepath.Join(a.cfg.DataDir, "uploads", strings.TrimPrefix(p, "/uploads/")))
		}
	}
}
func (a *app) serveImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !imageNamePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(a.cfg.DataDir, "uploads", name))
}
func (a *app) imageJob(r *http.Request) (func(), error) {
	if err := a.limit("image:"+a.keyed(a.clientIP(r)), 30, time.Hour); err != nil {
		return nil, err
	}
	select {
	case a.hashSlots <- struct{}{}:
		return func() { <-a.hashSlots }, nil
	default:
		return nil, problem(503, "Server busy. Try again shortly.")
	}
}
func decodeImage(data []byte) (image.Image, error) {
	if len(data) > maxImageBytes {
		return nil, problem(413, "Image must be at most 4 MiB.")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		return nil, problem(400, "Use a PNG, JPEG, or GIF image.")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 12000000 {
		return nil, problem(400, "Image exceeds 4096 pixels per side or 12 megapixels.")
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, problem(400, "Could not read image.")
	}
	return im, nil
}
func normalizeCrop(im image.Image, crop *cropRect) (cropRect, error) {
	w, h := im.Bounds().Dx(), im.Bounds().Dy()
	size := min(w, h)
	if crop == nil {
		return cropRect{(w - size) / 2, (h - size) / 2, size}, nil
	}
	if crop.X < 0 || crop.Y < 0 || crop.Size < 1 || crop.Size > w || crop.Size > h || crop.X > w-crop.Size || crop.Y > h-crop.Size {
		return cropRect{}, problem(400, "Crop must be a square within the image bounds.")
	}
	return *crop, nil
}
func flattenImage(im image.Image) *image.RGBA {
	full := image.NewRGBA(image.Rect(0, 0, im.Bounds().Dx(), im.Bounds().Dy()))
	draw.Draw(full, full.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(full, full.Bounds(), im, im.Bounds().Min, draw.Over)
	return full
}

// Area sampling averages all source pixels covered by each thumbnail pixel.
// This prevents fine details from aliasing when a large screenshot is reduced.
func makeThumbnail(full *image.RGBA, crop cropRect) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, thumbnailSize, thumbnailSize))
	scale := float64(crop.Size) / thumbnailSize
	for y := 0; y < thumbnailSize; y++ {
		top := float64(crop.Y) + float64(y)*scale
		bottom := top + scale
		for x := 0; x < thumbnailSize; x++ {
			left := float64(crop.X) + float64(x)*scale
			right := left + scale
			var red, green, blue, total float64
			for sy := int(top); sy < int(math.Ceil(bottom)) && sy < crop.Y+crop.Size; sy++ {
				wy := min(bottom, float64(sy+1)) - max(top, float64(sy))
				if wy <= 0 {
					continue
				}
				for sx := int(left); sx < int(math.Ceil(right)) && sx < crop.X+crop.Size; sx++ {
					weight := wy * (min(right, float64(sx+1)) - max(left, float64(sx)))
					if weight <= 0 {
						continue
					}
					i := full.PixOffset(sx, sy)
					red += float64(full.Pix[i]) * weight
					green += float64(full.Pix[i+1]) * weight
					blue += float64(full.Pix[i+2]) * weight
					total += weight
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i] = uint8(math.Round(red / total))
			out.Pix[i+1] = uint8(math.Round(green / total))
			out.Pix[i+2] = uint8(math.Round(blue / total))
			out.Pix[i+3] = 255
		}
	}
	return out
}
func encodeImage(im image.Image) ([]byte, error) {
	for _, quality := range []int{90, 75, 60, 45} {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if b.Len() <= maxImageBytes {
			return b.Bytes(), nil
		}
	}
	return nil, problem(413, "Image remains too large after compression.")
}
func writeImageFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func (a *app) storeImage(id int64, im image.Image, crop *cropRect, originalJPEG []byte) (storedImage, error) {
	c, err := normalizeCrop(im, crop)
	if err != nil {
		return storedImage{}, err
	}
	full := flattenImage(im)
	if originalJPEG == nil {
		originalJPEG, err = encodeImage(full)
		if err != nil {
			return storedImage{}, err
		}
	}
	thumb, err := encodeImage(makeThumbnail(full, c))
	if err != nil {
		return storedImage{}, err
	}
	url := fmt.Sprintf("/uploads/%d-%s.jpg", id, randomToken())
	result := storedImage{url, thumbnailURL(url), c}
	for _, file := range []struct {
		url  string
		data []byte
	}{{url, originalJPEG}, {result.ThumbnailURL, thumb}} {
		if err = writeImageFile(filepath.Join(a.cfg.DataDir, "uploads", strings.TrimPrefix(file.url, "/uploads/")), file.data); err != nil {
			a.removeImage(url, id)
			return storedImage{}, err
		}
	}
	return result, nil
}
func cropFromQuery(r *http.Request) (*cropRect, error) {
	q := r.URL.Query()
	if !q.Has("x") && !q.Has("y") && !q.Has("size") {
		return nil, nil
	}
	var c cropRect
	for key, out := range map[string]*int{"x": &c.X, "y": &c.Y, "size": &c.Size} {
		values := q[key]
		if len(values) != 1 {
			return nil, problem(400, "Provide x, y, and size for the crop.")
		}
		value, err := strconv.Atoi(values[0])
		if err != nil {
			return nil, problem(400, "Crop values must be integers.")
		}
		*out = value
	}
	return &c, nil
}
func (a *app) replaceImage(w http.ResponseWriter, i identity, m mod, media storedImage) error {
	res, err := a.db.Exec("UPDATE mods SET image_url=?,thumbnail_url=?,crop_x=?,crop_y=?,crop_size=? WHERE id=? AND image_url=?", media.ImageURL, media.ThumbnailURL, media.Crop.X, media.Crop.Y, media.Crop.Size, m.ID, m.ImageURL)
	if err != nil {
		a.removeImage(media.ImageURL, m.ID)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		a.removeImage(media.ImageURL, m.ID)
		return problem(409, "Mod changed. Reload and try again.")
	}
	a.removeImage(m.ImageURL, m.ID)
	result, err := a.readMod(m.ID, i.User.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, result)
}
func (a *app) uploadImage(w http.ResponseWriter, r *http.Request) error {
	i, m, err := a.ownedMod(r)
	if err != nil {
		return err
	}
	release, err := a.imageJob(r)
	if err != nil {
		return err
	}
	defer release()
	crop, err := cropFromQuery(r)
	if err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return problem(413, "Image must be at most 4 MiB.")
	}
	im, err := decodeImage(data)
	if err != nil {
		return err
	}
	media, err := a.storeImage(m.ID, im, crop, nil)
	if err != nil {
		return err
	}
	return a.replaceImage(w, i, m, media)
}
func (a *app) recropImage(w http.ResponseWriter, r *http.Request) error {
	i, m, err := a.ownedMod(r)
	if err != nil {
		return err
	}
	if !validLocalImage(m.ImageURL, m.ID) {
		return problem(400, "Upload or import an image first.")
	}
	release, err := a.imageJob(r)
	if err != nil {
		return err
	}
	defer release()
	var crop cropRect
	if err = decode(w, r, &crop); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "uploads", strings.TrimPrefix(m.ImageURL, "/uploads/")))
	if os.IsNotExist(err) {
		return problem(404, "Image not found.")
	}
	if err != nil {
		return err
	}
	im, err := decodeImage(data)
	if err != nil {
		return err
	}
	media, err := a.storeImage(m.ID, im, &crop, data)
	if err != nil {
		return err
	}
	return a.replaceImage(w, i, m, media)
}
func (a *app) previewImage(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.require(r, false); err != nil {
		return err
	}
	release, err := a.imageJob(r)
	if err != nil {
		return err
	}
	defer release()
	var in struct {
		URL string `json:"url"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	im, err := a.remoteImage(r.Context(), in.URL)
	if err != nil {
		return err
	}
	data, err := encodeImage(flattenImage(im))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, err = w.Write(data)
	return err
}
func (a *app) remoteImage(ctx context.Context, raw string) (image.Image, error) {
	data, err := fetchImage(ctx, a.imageClient, raw)
	if err != nil {
		return nil, err
	}
	return decodeImage(data)
}
func (a *app) importModImage(r *http.Request, raw string, id int64) (storedImage, error) {
	release, err := a.imageJob(r)
	if err != nil {
		return storedImage{}, err
	}
	defer release()
	im, err := a.remoteImage(r.Context(), raw)
	if err != nil {
		return storedImage{}, err
	}
	return a.storeImage(id, im, nil, nil)
}

// Upgrade old local uploads without fetching external URLs during startup.
func (a *app) backfillThumbnails() error {
	rows, err := a.db.Query("SELECT id,image_url FROM mods WHERE thumbnail_url='' AND image_url LIKE '/uploads/%'")
	if err != nil {
		return err
	}
	type oldImage struct {
		id  int64
		url string
	}
	list := []oldImage{}
	for rows.Next() {
		var im oldImage
		if err = rows.Scan(&im.id, &im.url); err != nil {
			rows.Close()
			return err
		}
		list = append(list, im)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, old := range list {
		if !validLocalImage(old.url, old.id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "uploads", strings.TrimPrefix(old.url, "/uploads/")))
		if err != nil {
			log.Printf("thumbnail missing source for mod %d", old.id)
			continue
		}
		im, err := decodeImage(data)
		if err != nil {
			log.Printf("thumbnail invalid source for mod %d", old.id)
			continue
		}
		media, err := a.storeImage(old.id, im, nil, data)
		if err != nil {
			return err
		}
		res, err := a.db.Exec("UPDATE mods SET image_url=?,thumbnail_url=?,crop_x=?,crop_y=?,crop_size=? WHERE id=? AND image_url=? AND thumbnail_url=''", media.ImageURL, media.ThumbnailURL, media.Crop.X, media.Crop.Y, media.Crop.Size, old.id, old.url)
		if err != nil {
			a.removeImage(media.ImageURL, old.id)
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			a.removeImage(media.ImageURL, old.id)
		} else {
			a.removeImage(old.url, old.id)
		}
	}
	return nil
}
