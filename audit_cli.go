package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func auditReadFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("expected a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > int(limit) {
		return nil, errors.New("file exceeds limit")
	}
	return b, err
}

func auditReadJSON(path string, out any) error {
	b, err := auditReadFile(path, 2<<20)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func auditWriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return auditWriteFile(path, append(b, '\n'))
}

func auditWriteFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".audit-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func fetchAuditPackage(ctx context.Context, raw string) ([]byte, error) {
	u, err := imageURL(raw) // Shared public-IP checks, pinned DNS and redirects.
	if err != nil {
		return nil, err
	}
	client := newImageClient()
	client.Timeout = 90 * time.Second
	client.Transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Second
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "he-mod-index/audit-pilot")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("download HTTP %d", res.StatusCode)
	}
	if res.ContentLength > auditMaxPackage {
		return nil, errors.New("download exceeds 64 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, auditMaxPackage+1))
	if len(b) > auditMaxPackage {
		return nil, errors.New("download exceeds 64 MiB")
	}
	return b, err
}

// This entry point deliberately precedes loadConfig/openDB in main. The worker
// sees only its own static executable and one read-only file in a new namespace.
func runAuditWorker(args []string) error {
	if len(args) != 1 || args[0] != "/package" {
		return errors.New("internal inspector expects /package")
	}
	for _, limit := range []struct {
		kind  int
		value uint64
	}{
		{syscall.RLIMIT_CPU, 30}, {syscall.RLIMIT_AS, 2 << 30}, {syscall.RLIMIT_NOFILE, 32}, {syscall.RLIMIT_CORE, 0},
	} {
		if err := syscall.Setrlimit(limit.kind, &syscall.Rlimit{Cur: limit.value, Max: limit.value}); err != nil {
			return err
		}
	}
	b, err := auditReadFile("/package", auditMaxPackage)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(inspectAuditBytes(b))
}

type auditBoundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *auditBoundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("worker output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func sandboxAuditInspect(ctx context.Context, file string) (auditInspection, error) {
	var in auditInspection
	// Copy a stable snapshot: neither a replaced download nor a concurrent write
	// may change the bytes between the broker's hash and the inspector's read.
	b, err := auditReadFile(file, auditMaxPackage)
	if err != nil {
		return in, err
	}
	tmp, err := os.MkdirTemp("", "he-audit-")
	if err != nil {
		return in, err
	}
	defer os.RemoveAll(tmp)
	packagePath := filepath.Join(tmp, "package")
	if err = os.WriteFile(packagePath, b, 0400); err != nil {
		return in, err
	}
	exe, err := os.Executable()
	if err != nil {
		return in, err
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bwrap", "--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", exe, "/inspector", "--ro-bind", packagePath, "/package", "--proc", "/proc", "--dev", "/dev",
		"--tmpfs", "/tmp", "--chdir", "/", "--setenv", "GOMEMLIMIT", "256MiB", "/inspector", "audit-worker", "/package")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	stdout, stderr := &auditBoundedBuffer{limit: 2 << 20}, &auditBoundedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err = cmd.Run(); err != nil {
		return in, fmt.Errorf("isolated inspector failed (no unsandboxed fallback): %w: %s", err, stderr.String())
	}
	if err = json.Unmarshal(stdout.Bytes(), &in); err != nil {
		return in, err
	}
	if in.SHA256 != auditHash(b) || in.Size != len(b) {
		return in, errors.New("worker artifact identity mismatch")
	}
	return in, validateAuditInspection(in)
}

func openAuditLedger(dir string) (*sql.DB, error) {
	path, err := filepath.Abs(filepath.Join(dir, "index.db"))
	if err != nil {
		return nil, err
	}
	// mode=rw refuses a typo/new database; this command never initializes schema
	// or accounts. The only SQL writes are real API usage reservations/charges.
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"rw"}, "_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("audit requires existing schema %d (found %d): %v", schemaVersion, version, err)
	}
	return db, nil
}

func runAudit(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: audit fetch|inspect|review|revalidate|status [flags]; see README")
	}
	fs := flag.NewFlagSet("audit "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	file := fs.String("file", "", "local package to inspect")
	raw := fs.String("url", "", "public download URL")
	output := fs.String("out", "", "output file")
	inspection := fs.String("inspection", "", "saved isolated inspection JSON")
	reportFile := fs.String("report", "", "saved report to revalidate offline")
	modelOutput := fs.String("model-output", "", "saved model output if the report failed validation")
	modID := fs.Int64("mod", 0, "existing listing ID (current edits/history read at review time)")
	contextFile := fs.String("context", "", "synthetic fixture context JSON instead of a listing")
	budget := fs.Float64("budget", 20, "cumulative shared ingestion + audit cap, at most $20")
	envFile := fs.String("env-file", "", "read only OPENAI_KEY/API_KEY from this file")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	switch args[0] {
	case "revalidate":
		if *inspection == "" || *reportFile == "" || *output == "" {
			return errors.New("revalidate requires --inspection, --report and --out")
		}
		// Explicit separate output preserves the original evidence trail, even
		// if an operator accidentally supplies the same file under another path.
		for _, input := range []string{*inspection, *reportFile, *modelOutput} {
			if input == "" {
				continue
			}
			inPath, err := filepath.Abs(input)
			if err != nil {
				return err
			}
			outPath, err := filepath.Abs(*output)
			if err != nil {
				return err
			}
			if inPath == outPath {
				return errors.New("revalidation output must not replace an input")
			}
			if outInfo, err := os.Stat(outPath); err == nil {
				inInfo, err := os.Stat(inPath)
				if err != nil {
					return err
				}
				if os.SameFile(inInfo, outInfo) {
					return errors.New("revalidation output must not replace an input")
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		var in auditInspection
		if err := auditReadJSON(*inspection, &in); err != nil {
			return err
		}
		var saved auditReport
		if err := auditReadJSON(*reportFile, &saved); err != nil {
			return err
		}
		result := saved.Result
		if *modelOutput != "" {
			result = new(auditModelResult)
			if err := auditReadJSON(*modelOutput, result); err != nil {
				return err
			}
		}
		report, reviewErr := revalidateAudit(in, saved, result)
		if err := auditWriteJSON(*output, report); err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(map[string]any{"sha256": report.SHA256, "state": report.State, "model_calls": 0, "validation_version": report.ValidationVersion}); err != nil {
			return err
		}
		return reviewErr
	case "status":
		db, err := openAuditLedger(env("DATA_DIR", "data"))
		if err != nil {
			return err
		}
		defer db.Close()
		var spent int64
		if err = db.QueryRow("SELECT COALESCE(SUM(COALESCE(actual_micros,reserved_micros)),0) FROM ingest_usage").Scan(&spent); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"schema_version": schemaVersion, "spent_or_reserved_micros": spent})
	case "fetch":
		if *raw == "" || *output == "" {
			return errors.New("fetch requires --url and --out")
		}
		b, err := fetchAuditPackage(ctx, *raw)
		if err != nil {
			return err
		}
		if err = auditWriteFile(*output, b); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"sha256": auditHash(b), "bytes": len(b), "url": *raw})
	case "inspect":
		if *file == "" || *output == "" {
			return errors.New("inspect requires --file and --out")
		}
		in, err := sandboxAuditInspect(ctx, *file)
		if err != nil {
			return err
		}
		if err = auditWriteJSON(*output, in); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"sha256": in.SHA256, "files": len(in.Files), "issues": in.Issues})
	case "review":
		if *inspection == "" || (*modID <= 0) == (*contextFile == "") || *output == "" {
			return errors.New("review requires --inspection, --out and either --mod or --context")
		}
		if math.IsNaN(*budget) || math.IsInf(*budget, 0) || *budget <= 0 || *budget > 20 {
			return errors.New("audit cumulative budget must be >0 and <=20 USD")
		}
		var in auditInspection
		if err := auditReadJSON(*inspection, &in); err != nil {
			return err
		}
		if err := validateAuditInspection(in); err != nil {
			return err
		}
		dir := env("DATA_DIR", "data")
		db, err := openAuditLedger(dir)
		if err != nil {
			return err
		}
		defer db.Close()
		a := &app{db: db, cfg: config{DataDir: dir}}
		unlock, err := a.ingestionLock()
		if err != nil {
			return err
		}
		defer unlock()
		var context auditContext
		if *modID > 0 {
			context, err = a.auditListingContext(ctx, *modID)
			if err == nil {
				match := false
				for _, d := range context.Listing.Downloads {
					if d.URL == *raw {
						match = true
					}
				}
				if !match {
					return errors.New("--url must still belong to the listing's current downloads")
				}
				context.ArtifactURL = *raw
			}
		} else {
			err = auditReadJSON(*contextFile, &context)
			if context.ModID != 0 {
				return errors.New("fixture context must have mod_id 0")
			}
		}
		if err != nil {
			return err
		}
		client, err := newModelClient(a, "gpt-6.2-astra", int64(math.Floor(*budget*1e6)), *envFile)
		if err != nil {
			return err
		}
		client.reasoning, client.maxOutput = "high", 12000
		client.client.Timeout = 8 * time.Minute
		client.client.Transport.(*http.Transport).ResponseHeaderTimeout = 8 * time.Minute
		report, reviewErr := reviewAudit(ctx, client, filepath.Join(dir, "audits"), in, context)
		if *modID > 0 {
			latest, readErr := a.auditListingContext(ctx, *modID)
			latest.ArtifactURL = *raw
			report.MetadataCurrent = readErr == nil && auditContextHash(latest) == report.ContextSHA256
		}
		if err = auditWriteJSON(*output, report); err != nil {
			return err
		}
		var spent int64
		if err = db.QueryRow("SELECT COALESCE(SUM(COALESCE(actual_micros,reserved_micros)),0) FROM ingest_usage").Scan(&spent); err != nil {
			return err
		}
		if err = json.NewEncoder(out).Encode(map[string]any{"sha256": report.SHA256, "state": report.State, "cached": report.Cached, "metadata_current": report.MetadataCurrent, "spent_or_reserved_usd": float64(spent) / 1e6, "cap_usd": *budget, "mod_id": strconv.FormatInt(*modID, 10)}); err != nil {
			return err
		}
		return reviewErr
	default:
		return errors.New("unknown audit command")
	}
}
