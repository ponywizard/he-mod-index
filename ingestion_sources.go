package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Posts are data, never instructions. IDs are shared between the live board and
// its archive, so seeing a post through both sources must not duplicate work.
type threadPost struct {
	ID       int64    `json:"id"`
	ThreadID int64    `json:"thread_id"`
	Time     int64    `json:"time"`
	Author   string   `json:"author"`
	Text     string   `json:"text"`
	URL      string   `json:"url"`
	Image    string   `json:"image"`
	Links    []string `json:"links"`
	Replies  []int64  `json:"replies"`
}

var postTags = regexp.MustCompile(`<[^>]*>`)
var postBreaks = regexp.MustCompile(`(?i)<br\s*/?>`)
var postURLs = regexp.MustCompile(`https?://[^\s<>"\[\]]+`)
var postReplies = regexp.MustCompile(`>>(\d{5,})`)

func postText(s string) string {
	s = postBreaks.ReplaceAllString(s, "\n")
	return strings.TrimSpace(html.UnescapeString(postTags.ReplaceAllString(s, "")))
}
func normalizePost(p threadPost) threadPost {
	p.Text = postText(p.Text)
	if len(p.Text) > 30000 {
		p.Text = p.Text[:30000]
	}
	p.Author = postText(p.Author)
	if p.Author == "" {
		p.Author = "Anonymous"
	}
	if len(p.Author) > 100 {
		p.Author = "Anonymous"
	}
	seen := map[string]bool{}
	for _, raw := range postURLs.FindAllString(p.Text, -1) {
		raw = strings.TrimRight(raw, ".,;!?)")
		if validURL(raw) && !seen[raw] {
			p.Links = append(p.Links, raw)
			seen[raw] = true
		}
	}
	for _, m := range postReplies.FindAllStringSubmatch(p.Text, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		p.Replies = append(p.Replies, id)
	}
	return p
}

type livePost struct {
	ID        int64  `json:"no"`
	Parent    int64  `json:"resto"`
	Time      int64  `json:"time"`
	Name      string `json:"name"`
	Subject   string `json:"sub"`
	Comment   string `json:"com"`
	Image     int64  `json:"tim"`
	Extension string `json:"ext"`
}

func parseLiveThread(data []byte) ([]threadPost, error) {
	var body struct {
		Posts []livePost `json:"posts"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	if len(body.Posts) == 0 {
		return nil, fmt.Errorf("empty thread")
	}
	out := []threadPost{}
	for _, p := range body.Posts {
		tid := p.Parent
		if tid == 0 {
			tid = p.ID
		}
		q := threadPost{ID: p.ID, ThreadID: tid, Time: p.Time, Author: p.Name, Text: p.Subject + "\n" + p.Comment, URL: fmt.Sprintf("https://boards.4chan.org/mlp/thread/%d#p%d", tid, p.ID)}
		if p.Image > 0 && (p.Extension == ".png" || p.Extension == ".jpg" || p.Extension == ".gif") {
			q.Image = fmt.Sprintf("https://i.4cdn.org/mlp/%d%s", p.Image, p.Extension)
		}
		out = append(out, normalizePost(q))
	}
	return out, nil
}

type archivePost struct {
	ID       json.Number `json:"num"`
	ThreadID json.Number `json:"thread_num"`
	Subnum   json.Number `json:"subnum"`
	Time     int64       `json:"timestamp"`
	Name     string      `json:"name"`
	Subject  string      `json:"title"`
	Comment  string      `json:"comment"`
	Media    *struct {
		Link string `json:"media_link"`
	} `json:"media"`
}

func (p archivePost) post() threadPost {
	id, _ := p.ID.Int64()
	tid, _ := p.ThreadID.Int64()
	q := threadPost{ID: id, ThreadID: tid, Time: p.Time, Author: p.Name, Text: p.Subject + "\n" + p.Comment, URL: fmt.Sprintf("https://desuarchive.org/mlp/thread/%d/#%d", tid, id)}
	if p.Media != nil {
		q.Image = p.Media.Link
	}
	return normalizePost(q)
}
func parseArchiveThread(data []byte, id int64) ([]threadPost, error) {
	var body map[string]struct {
		OP    archivePost            `json:"op"`
		Posts map[string]archivePost `json:"posts"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	thread, ok := body[strconv.FormatInt(id, 10)]
	if !ok {
		return nil, fmt.Errorf("archive omitted thread %d", id)
	}
	out := []threadPost{thread.OP.post()}
	for _, p := range thread.Posts {
		if string(p.Subnum) != "0" && string(p.Subnum) != "" {
			continue
		}
		out = append(out, p.post())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type threadFetcher struct {
	client *http.Client
	last   time.Time
}

func newThreadFetcher() *threadFetcher {
	c := newImageClient()
	c.Timeout = 30 * time.Second
	c.Transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Second
	return &threadFetcher{client: c}
}
func (f *threadFetcher) get(ctx context.Context, raw, modified string) ([]byte, string, int, error) {
	// One request per second, including catalog/thread transitions. Use conditional
	// requests for unchanged live threads as requested by the board's API rules.
	if delay := time.Until(f.last.Add(1100 * time.Millisecond)); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, "", 0, ctx.Err()
		case <-timer.C:
		}
	}
	f.last = time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", "he-mod-index/1.0")
	req.Header.Set("Accept", "application/json")
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == 304 || res.StatusCode == 404 {
		return nil, "", res.StatusCode, nil
	}
	if res.StatusCode != 200 {
		return nil, "", res.StatusCode, fmt.Errorf("source returned HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 12<<20+1))
	if err != nil {
		return nil, "", res.StatusCode, err
	}
	if len(b) > 12<<20 {
		return nil, "", res.StatusCode, fmt.Errorf("source response exceeds 12 MiB")
	}
	if !json.Valid(b) {
		return nil, "", res.StatusCode, fmt.Errorf("source did not return JSON")
	}
	return b, res.Header.Get("Last-Modified"), res.StatusCode, nil
}
func (f *threadFetcher) archiveIDs(ctx context.Context, start string) ([]int64, error) {
	seen := map[int64]bool{}
	out := []int64{}
	for page := 1; page <= 100; page++ {
		raw := "https://desuarchive.org/_/api/chan/search/?boards=mlp&subject=" + url.QueryEscape("Home: Equestria") + "&start=" + url.QueryEscape(start) + "&page=" + strconv.Itoa(page)
		b, _, _, err := f.get(ctx, raw, "")
		if err != nil {
			return nil, err
		}
		var result map[string]json.RawMessage
		if err = json.Unmarshal(b, &result); err != nil {
			return nil, err
		}
		if result["error"] != nil {
			return nil, fmt.Errorf("archive search returned an error")
		}
		var group struct {
			Posts []archivePost `json:"posts"`
		}
		if err = json.Unmarshal(result["0"], &group); err != nil {
			return nil, err
		}
		var meta struct {
			Total int `json:"total_found"`
		}
		_ = json.Unmarshal(result["meta"], &meta)
		added := 0
		for _, p := range group.Posts {
			id, _ := p.ThreadID.Int64()
			if id > 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
				added++
			}
		}
		if len(group.Posts) == 0 || len(out) >= meta.Total && meta.Total > 0 {
			sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
			return out, nil
		}
		if added == 0 {
			return nil, fmt.Errorf("archive pagination repeated before all results were fetched")
		}
	}
	return nil, fmt.Errorf("archive search exceeded 100 pages")
}
func currentThreadFromCatalog(data []byte, current int64) (int64, error) {
	var pages []struct {
		Threads []livePost `json:"threads"`
	}
	if err := json.Unmarshal(data, &pages); err != nil {
		return 0, err
	}
	for _, page := range pages {
		for _, p := range page.Threads {
			subject := strings.ToLower(postText(p.Subject))
			if strings.HasPrefix(subject, "home: equestria") && p.ID > current {
				current = p.ID
			}
		}
	}
	return current, nil
}
