package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"time"
)

// Frozen verbatim from the original Astra pilot (1b188cd). Comparing models
// changes only the model, not the package, history, editorial context or prompt.
// The corrected evidence validator is shared by both models' results.
//
//go:embed testdata/audit/instructions-v1.txt
var auditComparisonPrompt string

type auditComparisonOptions struct {
	Inspection, Baseline, Output, Model, EnvFile string
	Budget                                       float64
}

func auditComparisonBaseline(in auditInspection, baseline auditReport) error {
	if err := validateAuditInspection(in); err != nil {
		return err
	}
	if baseline.Policy != auditPolicy || baseline.SHA256 != in.SHA256 || baseline.ContextSHA256 != auditContextHash(baseline.Context) {
		return errors.New("comparison baseline artifact, policy or context identity mismatch")
	}
	if baseline.Model != "gpt-6.2-astra" || baseline.Reasoning != "high" {
		return errors.New("comparison requires the saved Astra high-reasoning pilot baseline")
	}
	_, inputHash, err := auditReviewInput(in, baseline.Context, auditComparisonPrompt)
	if err != nil {
		return err
	}
	if baseline.InputSHA256 != "" && baseline.InputSHA256 != inputHash {
		return errors.New("baseline used different prompt or request settings")
	}
	return nil
}

func runAuditComparison(ctx context.Context, o auditComparisonOptions, out io.Writer) error {
	if o.Inspection == "" || o.Baseline == "" || o.Output == "" {
		return errors.New("compare requires --inspection, --report and --out")
	}
	if o.Model != "gpt-6.2-sol" {
		return errors.New("comparison model must be gpt-6.2-sol")
	}
	if math.IsNaN(o.Budget) || math.IsInf(o.Budget, 0) || o.Budget <= 0 || o.Budget > 20 {
		return errors.New("audit cumulative budget must be >0 and <=20 USD")
	}
	if err := auditSeparateOutput(o.Output, o.Inspection, o.Baseline); err != nil {
		return err
	}
	var in auditInspection
	if err := auditReadJSON(o.Inspection, &in); err != nil {
		return err
	}
	var baseline auditReport
	if err := auditReadJSON(o.Baseline, &baseline); err != nil {
		return err
	}
	if err := auditComparisonBaseline(in, baseline); err != nil {
		return err
	}
	baselineBytes, err := json.Marshal(baseline)
	if err != nil {
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
	c, err := newModelClient(a, o.Model, int64(math.Floor(o.Budget*1e6)), o.EnvFile)
	if err != nil {
		return err
	}
	c.reasoning, c.maxOutput, c.auditPrompt = "high", 12000, auditComparisonPrompt
	c.client.Timeout = 8 * time.Minute
	c.client.Transport.(*http.Transport).ResponseHeaderTimeout = 8 * time.Minute
	// Deliberate model comparison has its own result namespace, not a new usage
	// ledger. Original Astra reports and interrupted attempts stay untouched.
	cache := filepath.Join(dir, "audits", "comparisons", o.Model)
	r, reviewErr := reviewAudit(ctx, c, cache, in, baseline.Context)
	r.BaselineModel, r.BaselineReportSHA256 = baseline.Model, auditHash(baselineBytes)
	r.MetadataCurrent = baseline.Context.ModID == 0 // Frozen context, not live edits.
	if err := auditWriteJSON(o.Output, r); err != nil {
		return err
	}
	var spent int64
	if err := db.QueryRow("SELECT COALESCE(SUM(COALESCE(actual_micros,reserved_micros)),0) FROM ingest_usage").Scan(&spent); err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(map[string]any{"model": r.Model, "sha256": r.SHA256, "state": r.State, "cached": r.Cached, "frozen_context": true, "spent_or_reserved_usd": float64(spent) / 1e6, "cap_usd": o.Budget, "usage": r.Usage}); err != nil {
		return err
	}
	return reviewErr
}
