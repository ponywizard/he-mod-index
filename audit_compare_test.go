package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditComparisonFrozenInputsAndIndependentResults(t *testing.T) {
	if got := auditHash([]byte(auditComparisonPrompt)); got != "832805be90323f0810f69cf23f377e075f4625c3f1854d230bd4dbf83f78cc43" {
		t.Fatal("original pilot prompt changed", got)
	}
	a := testApp(t)
	in := inspectAuditBytes([]byte("extends Node\n"))
	c := auditContext{ModID: 3, Listing: modInput{Name: "Operator correction", ShortDescription: "Frozen description"}}
	response := auditTestResult(in, c)
	requests := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if body["prompt_cache_options"].(map[string]any)["mode"] != "explicit" || body["instructions"] != auditComparisonPrompt || body["max_output_tokens"] != float64(12000) || body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Fatal("comparison settings differ", body)
		}
		b, _ := json.Marshal(response)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(b)}}}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}})
	}))
	defer server.Close()
	cache := t.TempDir()
	astra := &modelClient{app: a, model: "gpt-6.2-astra", key: "test", budget: 20e6, endpoint: server.URL, client: server.Client(), reasoning: "high", maxOutput: 12000, auditPrompt: auditComparisonPrompt}
	baseline, err := reviewAudit(context.Background(), astra, cache, in, c)
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(cache, in.SHA256, "report.json")
	before, _ := os.ReadFile(baselinePath)
	sol := &modelClient{app: a, model: "gpt-6.2-sol", key: "test", budget: 20e6, endpoint: server.URL, client: server.Client(), reasoning: "high", maxOutput: 12000, auditPrompt: auditComparisonPrompt}
	comparisonCache := filepath.Join(cache, "comparisons", sol.model)
	comparison, err := reviewAudit(context.Background(), sol, comparisonCache, in, c)
	if err != nil || comparison.Model != sol.model || comparison.Cached || len(requests) != 2 {
		t.Fatal(comparison, err, len(requests))
	}
	if baseline.InputSHA256 != comparison.InputSHA256 {
		t.Fatal("different comparison input fingerprints")
	}
	// The actual HTTP payload differs only in model ID, including schema/options.
	delete(requests[0], "model")
	delete(requests[1], "model")
	one, _ := json.Marshal(requests[0])
	two, _ := json.Marshal(requests[1])
	if !bytes.Equal(one, two) {
		t.Fatal("comparison altered more than the model")
	}
	if comparison.Usage == nil || baseline.Usage == nil || comparison.Usage.ActualMicros == nil || baseline.Usage.ActualMicros == nil || *comparison.Usage.ActualMicros*4 != *baseline.Usage.ActualMicros || comparison.Usage.ID == baseline.Usage.ID {
		t.Fatal("incorrect usage attribution", baseline.Usage, comparison.Usage)
	}
	var spent int64
	if err := a.db.QueryRow("SELECT SUM(actual_micros) FROM ingest_usage").Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent != *baseline.Usage.ActualMicros+*comparison.Usage.ActualMicros {
		t.Fatal("separate spending ledger", spent)
	}
	duplicate, err := reviewAudit(context.Background(), sol, comparisonCache, in, c)
	if err != nil || !duplicate.Cached || len(requests) != 2 || duplicate.Usage.ID != comparison.Usage.ID {
		t.Fatal("duplicate charged", duplicate, err)
	}
	if _, err := reviewAudit(context.Background(), sol, cache, in, c); err == nil {
		t.Fatal("returned Astra result for Sol")
	}
	c.Listing.Description = "Newer edit outside the frozen comparison"
	if _, err := reviewAudit(context.Background(), sol, comparisonCache, in, c); err == nil || len(requests) != 2 {
		t.Fatal("changed frozen context reused or recharged", err)
	}
	after, _ := os.ReadFile(baselinePath)
	if !bytes.Equal(before, after) {
		t.Fatal("baseline overwritten")
	}
	// An encryption-only result must not inherit the previous call's charge.
	sol.lastUsage = comparison.Usage
	encrypted := inspectAuditBytes(auditTestPCK(3, 3, 0, "res://main.gd", []byte("extends Node")))
	blocked, err := reviewAudit(context.Background(), sol, comparisonCache, encrypted, c)
	if err != nil || blocked.State != "red_flag" || blocked.Usage != nil || len(requests) != 2 {
		t.Fatal("encrypted artifact incurred usage", blocked, err)
	}
}

func TestAuditComparisonRejectsIncorrectBaselineAndCap(t *testing.T) {
	in := inspectAuditBytes([]byte("extends Node\n"))
	c := auditContext{Listing: modInput{Name: "Fixture", ShortDescription: "Frozen description"}}
	baseline := auditReport{Model: "gpt-6.2-astra", Reasoning: "high", Policy: auditPolicy, SHA256: in.SHA256, Context: c, ContextSHA256: auditContextHash(c)}
	if err := auditComparisonBaseline(in, baseline); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*auditReport){
		func(r *auditReport) { r.SHA256 = strings.Repeat("0", 64) },
		func(r *auditReport) { r.Context.Listing.Name = "tampered" },
		func(r *auditReport) { r.Model = "gpt-6.2-sol" },
		func(r *auditReport) { r.Reasoning = "low" },
		func(r *auditReport) { r.InputSHA256 = strings.Repeat("0", 64) },
	} {
		bad := baseline
		mutate(&bad)
		if err := auditComparisonBaseline(in, bad); err == nil {
			t.Fatal("accepted altered baseline")
		}
	}
	for _, cap := range []string{"21", "NaN", "+Inf", "0"} {
		args := []string{"compare", "--inspection", "missing", "--report", "missing", "--out", "other", "--budget", cap}
		if err := runAudit(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatal(cap, err)
		}
	}
	if err := runAudit(context.Background(), []string{"review", "--model", "gpt-6.2-sol"}, &bytes.Buffer{}); err == nil {
		t.Fatal("silently ignored selected model")
	}
}
