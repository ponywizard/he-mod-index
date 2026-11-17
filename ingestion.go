package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ingestOptions struct {
	evalExtract, dryRun                                                 bool
	command, model, extractModel, envFile, start, sources, labels, from string
	owner                                                               int64
	budget                                                              float64
	collectOnly                                                         bool
}

func parseIngest(args []string, out io.Writer) (ingestOptions, error) {
	o := ingestOptions{}
	if len(args) == 0 {
		args = []string{"--help"}
	}
	o.command = args[0]
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&o.evalExtract, "extract", false, "evaluate extraction instead of classification")
	fs.BoolVar(&o.dryRun, "dry-run", false, "preview prune decisions without changing the queue")
	fs.StringVar(&o.extractModel, "extract-model", "gpt-5.4-mini", "model for extraction and history-based descriptions")
	fs.StringVar(&o.model, "model", "gpt-6-luna", "OpenAI model with verified price")
	fs.StringVar(&o.envFile, "env-file", "", "read only OPENAI_KEY/API_KEY from this file")
	fs.StringVar(&o.start, "start", "2026-08-07", "archive search start (YYYY-MM-DD)")
	fs.StringVar(&o.sources, "sources", "", "read saved archive JSON files from a directory")
	fs.StringVar(&o.from, "from", "", "staging SQLite database for merge")
	fs.StringVar(&o.labels, "labels", "", "eval JSON: array of {post_id,release}")
	fs.Int64Var(&o.owner, "owner", 0, "existing admin owner ID (0 stages only, or summarizes/backfills all imports)")
	fs.Float64Var(&o.budget, "budget", 5, "cumulative API spending cap in USD for this database")
	fs.BoolVar(&o.collectOnly, "collect-only", false, "collect source posts without API calls or publication")
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: he-mod-index ingest hydrate|poll|process|status|eval|merge|summarize|images|prune [flags]\nUses DATA_DIR. hydrate collects archive threads; poll discovers and reads live threads.\nprocess resumes saved work; eval measures classifier recall/precision on labeled posts.")
		fs.PrintDefaults()
	}
	if o.command == "--help" || o.command == "-h" {
		fs.Usage()
		return o, flag.ErrHelp
	}
	if o.command != "hydrate" && o.command != "poll" && o.command != "process" && o.command != "status" && o.command != "eval" && o.command != "merge" && o.command != "summarize" && o.command != "images" && o.command != "prune" {
		return o, errors.New("unknown ingest command; use ingest --help")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, errors.New("unexpected ingest arguments")
	}
	if o.budget <= 0 || o.budget > 1000 || o.owner < 0 {
		return o, errors.New("budget must be >0 and <=1000; owner must be nonnegative")
	}
	if _, err := time.Parse("2006-01-02", o.start); err != nil {
		return o, errors.New("start must be YYYY-MM-DD")
	}
	if o.command == "merge" && o.from == "" {
		return o, errors.New("merge requires --from")
	}
	if o.command == "eval" && o.labels == "" {
		return o, errors.New("eval requires --labels")
	}
	return o, nil
}
func (a *app) ingestionLock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(a.cfg.DataDir, "ingest.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another ingestion run holds the data-directory lock")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func (a *app) hydrate(ctx context.Context, o ingestOptions) error {
	if o.sources != "" {
		paths, err := filepath.Glob(filepath.Join(o.sources, "*.json"))
		if err != nil {
			return err
		}
		count := 0
		for _, path := range paths {
			id, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(path), ".json"), 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			posts, err := parseArchiveThread(b, id)
			if err != nil {
				return err
			}
			if err = a.savePosts(ctx, posts); err != nil {
				return err
			}
			count++
		}
		if count == 0 {
			return errors.New("no numeric archive JSON filenames found")
		}
		return nil
	}
	f := newThreadFetcher()
	ids, err := f.archiveIDs(ctx, o.start)
	if err != nil {
		return err
	}
	for _, id := range ids {
		b, _, status, err := f.get(ctx, fmt.Sprintf("https://desuarchive.org/_/api/chan/thread/?board=mlp&num=%d", id), "")
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("archive thread %d returned HTTP %d", id, status)
		}
		posts, err := parseArchiveThread(b, id)
		if err != nil {
			return err
		}
		if err = a.savePosts(ctx, posts); err != nil {
			return err
		}
		log.Printf("archive thread %d: %d posts collected", id, len(posts))
	}
	return nil
}
func (a *app) pollThreads(ctx context.Context) error {
	return a.pollThreadsWith(ctx, newThreadFetcher())
}
func (a *app) pollThreadsWith(ctx context.Context, f *threadFetcher) error {
	var raw string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='current_thread'").Scan(&raw); err != nil {
		return err
	}
	current, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return err
	}
	b, _, status, err := f.get(ctx, "https://a.4cdn.org/mlp/catalog.json", "")
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("catalog returned HTTP %d", status)
	}
	newest, err := currentThreadFromCatalog(b, current)
	if err != nil {
		return err
	}
	// Continue all known threads until they disappear, including a predecessor
	// still receiving posts after a new OP appears. Seed the previous thread too.
	ids := []int64{current}
	if newest != current {
		ids = append(ids, newest)
	}
	var pages []struct {
		Threads []livePost `json:"threads"`
	}
	_ = json.Unmarshal(b, &pages)
	for _, page := range pages {
		for _, p := range page.Threads {
			if p.ID != current && p.ID != newest && strings.HasPrefix(strings.ToLower(postText(p.Subject)), "home: equestria") {
				ids = append(ids, p.ID)
			}
		}
	}
	// Remember active predecessors so their final posts are fetched even after
	// they disappear from the catalog. Search the archive on rollover to cover
	// intermediate threads created during downtime.
	watched, err := a.db.Query("SELECT key FROM settings WHERE key LIKE 'thread_watch:%' AND value='active'")
	if err != nil {
		return err
	}
	for watched.Next() {
		var key string
		if err = watched.Scan(&key); err != nil {
			watched.Close()
			return err
		}
		id, e := strconv.ParseInt(strings.TrimPrefix(key, "thread_watch:"), 10, 64)
		if e != nil {
			watched.Close()
			return e
		}
		ids = append(ids, id)
	}
	err = watched.Err()
	watched.Close()
	if err != nil {
		return err
	}
	if newest > current {
		archived, err := f.archiveIDs(ctx, "2026-08-07")
		if err != nil {
			return err
		}
		for _, id := range archived {
			if id > current && id < newest {
				ids = append(ids, id)
			}
		}
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		watchKey := fmt.Sprintf("thread_watch:%d", id)
		if _, err = a.db.Exec("INSERT INTO settings(key,value) VALUES(?,'active') ON CONFLICT(key) DO NOTHING", watchKey); err != nil {
			return err
		}
		watchState := "active"
		key := fmt.Sprintf("thread_modified:%d", id)
		modified := ""
		err = a.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&modified)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		data, last, status, err := f.get(ctx, fmt.Sprintf("https://a.4cdn.org/mlp/thread/%d.json", id), modified)
		if err != nil {
			return err
		}
		if status == 304 {
			continue
		}
		if status == 404 {
			watchState = "closed"
			data, _, status, err = f.get(ctx, fmt.Sprintf("https://desuarchive.org/_/api/chan/thread/?board=mlp&num=%d", id), "")
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("thread %d unavailable live and in archive", id)
			}
			posts, err := parseArchiveThread(data, id)
			if err != nil {
				return err
			}
			if err = a.savePosts(ctx, posts); err != nil {
				return err
			}
		} else {
			posts, err := parseLiveThread(data)
			if err != nil {
				return err
			}
			if err = a.savePosts(ctx, posts); err != nil {
				return err
			}
		}
		// Save the conditional header only after the source posts are durable. Model
		// failures cannot strand already collected posts behind a 304 response.
		if _, err = a.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, last); err != nil {
			return err
		}
		if _, err = a.db.Exec("UPDATE settings SET value=? WHERE key=?", watchState, watchKey); err != nil {
			return err
		}
	}
	_, err = a.db.Exec("UPDATE settings SET value=? WHERE key='current_thread'", strconv.FormatInt(newest, 10))
	return err
}
func (a *app) publishPending(ctx context.Context, owner int64) error {
	if _, err := a.prunePendingImports(ctx, false); err != nil {
		return err
	}
	rows, err := a.db.Query(candidateSelect + " WHERE c.state='pending' ORDER BY c.post_id,c.ordinal")
	if err != nil {
		return err
	}
	list := []importCandidate{}
	for rows.Next() {
		c, e := candidateFrom(rows)
		if e != nil {
			rows.Close()
			return e
		}
		list = append(list, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	published, held := 0, 0
	for _, c := range list {
		id, err := a.publishCandidate(ctx, c.ID, owner, nil, nil, 0)
		if err != nil {
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.status != 409 {
				return err
			}
			if _, err = a.db.Exec("UPDATE ingest_candidates SET reason=? WHERE id=? AND state='pending'", apiErr.message, c.ID); err != nil {
				return err
			}
			held++
			continue
		}
		published++
		if err = a.importCandidateImage(ctx, c, id, owner, nil); err != nil {
			log.Printf("mod %d saved; image import failed: %v", id, err)
		}
	}
	log.Printf("ingestion: %d published, %d held", published, held)
	return nil
}
func (a *app) ingestionStatus(out io.Writer) error {
	stats := map[string]any{}
	for _, table := range []string{"ingest_posts", "ingest_candidates"} {
		rows, err := a.db.Query("SELECT state,COUNT(*) FROM " + table + " GROUP BY state")
		if err != nil {
			return err
		}
		states := map[string]int{}
		for rows.Next() {
			var state string
			var n int
			if err = rows.Scan(&state, &n); err != nil {
				rows.Close()
				return err
			}
			states[state] = n
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		stats[table] = states
	}
	var spent int64
	if err := a.db.QueryRow("SELECT COALESCE(SUM(COALESCE(actual_micros,reserved_micros)),0) FROM ingest_usage").Scan(&spent); err != nil {
		return err
	}
	stats["api_spent_or_reserved_usd"] = float64(spent) / 1e6
	return json.NewEncoder(out).Encode(stats)
}
func (a *app) runIngest(ctx context.Context, o ingestOptions, out io.Writer) error {
	unlock, err := a.ingestionLock()
	if err != nil {
		return err
	}
	defer unlock()
	if o.command == "status" {
		return a.ingestionStatus(out)
	}
	if o.owner > 0 {
		var role string
		if err = a.db.QueryRow("SELECT role FROM users WHERE id=?", o.owner).Scan(&role); err != nil || role != "admin" {
			return errors.New("--owner must identify an existing admin account")
		}
	}
	if o.command == "prune" {
		decisions, err := a.prunePendingImports(ctx, o.dryRun)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(decisions)
	}
	if o.command == "images" {
		if err = a.backfillGallery(ctx, o.owner); err != nil {
			return err
		}
		return a.ingestionStatus(out)
	}
	if o.command == "summarize" {
		c, err := newModelClient(a, o.extractModel, int64(o.budget*1e6), o.envFile)
		if err != nil {
			return err
		}
		if err = a.summarizePending(ctx, c, o.owner); err != nil {
			return err
		}
		return a.ingestionStatus(out)
	}
	if o.command == "merge" {
		if err = a.mergeIngestion(ctx, o.from); err != nil {
			return err
		}
		if o.owner > 0 {
			if err = a.publishPending(ctx, o.owner); err != nil {
				return err
			}
		}
		return a.ingestionStatus(out)
	}
	if o.command == "hydrate" {
		if err = a.hydrate(ctx, o); err != nil {
			return err
		}
	}
	if o.command == "poll" {
		if err = a.pollThreads(ctx); err != nil {
			return err
		}
	}
	if o.collectOnly {
		return a.ingestionStatus(out)
	}
	c, err := newModelClient(a, o.model, int64(o.budget*1e6), o.envFile)
	if err != nil {
		return err
	}
	if o.command == "eval" {
		return a.evalClassifier(ctx, c, o.labels, out, o.evalExtract)
	}
	if err = a.classifyPending(ctx, c); err != nil {
		return err
	}
	extractor, err := newModelClient(a, o.extractModel, int64(o.budget*1e6), o.envFile)
	if err != nil {
		return err
	}
	if err = a.extractPending(ctx, extractor); err != nil {
		return err
	}
	if o.owner > 0 {
		if err = a.publishPending(ctx, o.owner); err != nil {
			return err
		}
		if err = a.summarizePending(ctx, extractor, o.owner); err != nil {
			return err
		}
	}
	return a.ingestionStatus(out)
}
func (a *app) evalClassifier(ctx context.Context, c *modelClient, path string, out io.Writer, extract bool) error {
	var labels []struct {
		PostID  int64 `json:"post_id"`
		Release bool  `json:"release"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &labels); err != nil {
		return err
	}
	if len(labels) == 0 || len(labels) > 100 {
		return errors.New("eval requires 1–100 labels")
	}
	wanted := map[int64]bool{}
	predicted := map[int64]bool{}
	posts := []threadPost{}
	for _, l := range labels {
		if _, ok := wanted[l.PostID]; ok {
			return errors.New("duplicate evaluation label")
		}
		wanted[l.PostID] = l.Release
		var b string
		if err = a.db.QueryRow("SELECT payload FROM ingest_posts WHERE id=?", l.PostID).Scan(&b); err != nil {
			return err
		}
		var p threadPost
		if err = json.Unmarshal([]byte(b), &p); err != nil {
			return err
		}
		posts = append(posts, p)
	}
	extractionNames := map[int64][]string{}
	extractionDetails := map[int64][]extractedMod{}
	extractionErrors := map[int64]string{}
	if extract {
		for _, p := range posts {
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
				extractionErrors[p.ID] = err.Error()
				continue
			}
			predicted[p.ID] = len(entries) > 0
			extractionDetails[p.ID] = entries
			for _, e := range entries {
				extractionNames[p.ID] = append(extractionNames[p.ID], e.Name)
			}
		}
	} else {
		for start := 0; start < len(posts); start += 12 {
			batch := posts[start:min(start+12, len(posts))]
			contextPosts, err := a.postContext(batch)
			if err != nil {
				return err
			}
			result, err := c.classify(ctx, batch, contextPosts)
			if err != nil {
				return err
			}
			for _, r := range result.Candidates {
				predicted[r.PostID] = true
			}
		}
	}
	tp, fp, fn := 0, 0, 0
	misses := []int64{}
	falsePositives := []int64{}
	for id, want := range wanted {
		switch {
		case want && predicted[id]:
			tp++
		case want:
			fn++
			misses = append(misses, id)
		case predicted[id]:
			fp++
			falsePositives = append(falsePositives, id)
		}
	}
	return json.NewEncoder(out).Encode(map[string]any{"model": c.model, "labeled": len(labels), "true_positives": tp, "false_positives": fp, "false_negatives": fn, "missed_posts": misses, "false_positive_posts": falsePositives, "extractions": extractionNames, "details": extractionDetails, "errors": extractionErrors})
}

func (a *app) pollWorker(ctx context.Context) {
	if a.cfg.IngestInterval == 0 {
		return
	}
	ticker := time.NewTicker(a.cfg.IngestInterval)
	defer ticker.Stop()
	for {
		if err := a.runIngest(ctx, a.cfg.IngestOptions, io.Discard); err != nil && ctx.Err() == nil {
			log.Printf("thread polling: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
