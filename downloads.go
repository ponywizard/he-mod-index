package main

import (
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"
)

type download struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

func downloadFilename(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" || len(name) > 180 || strings.ContainsAny(name, "/\\") || strings.ContainsFunc(name, unicode.IsControl) {
		return "download"
	}
	return name
}

func (in *modInput) validateDownloads() error {
	// Accept the original single-link API for existing clients. Responses and the UI
	// always use downloads; the legacy value mirrors the first item.
	if in.Downloads == nil && in.DownloadURL != "" {
		in.Downloads = []download{{downloadFilename(in.DownloadURL), in.DownloadURL}}
	}
	if len(in.Downloads) < 1 || len(in.Downloads) > 20 {
		return problem(400, "Add between 1 and 20 download links.")
	}
	urls := map[string]bool{}
	for i := range in.Downloads {
		d := &in.Downloads[i]
		d.Filename = strings.TrimSpace(d.Filename)
		d.URL = strings.TrimSpace(d.URL)
		if len(d.Filename) < 1 || len(d.Filename) > 180 || d.Filename == "." || d.Filename == ".." || strings.ContainsAny(d.Filename, "/\\") || strings.ContainsFunc(d.Filename, unicode.IsControl) {
			return problem(400, "Each download needs a filename (1–180 bytes, without slashes or control characters).")
		}
		if !validURL(d.URL) {
			return problem(400, "Each download needs a valid http(s) URL.")
		}
		if urls[d.URL] {
			return problem(400, "Duplicate download URL.")
		}
		urls[d.URL] = true
	}
	in.DownloadURL = in.Downloads[0].URL
	return nil
}

func sameReleaseDownloads(a, b []download) bool {
	if len(a) != len(b) {
		return false
	}
	urls := func(list []download) []string {
		out := make([]string, len(list))
		for i, d := range list {
			out[i] = d.URL
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(urls(a), urls(b))
}
