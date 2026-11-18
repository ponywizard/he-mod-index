package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditTestZip(t *testing.T, names, contents []string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(contents[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func auditTestPCK(version, flags, fileFlags uint32, name string, contents []byte) []byte {
	header := make([]byte, 128)
	copy(header, "GDPC")
	binary.LittleEndian.PutUint32(header[4:], version)
	binary.LittleEndian.PutUint32(header[8:], 4)
	binary.LittleEndian.PutUint32(header[20:], flags)
	binary.LittleEndian.PutUint64(header[24:], 128)
	var directory bytes.Buffer
	put := func(v any) { _ = binary.Write(&directory, binary.LittleEndian, v) }
	put(uint32(1))
	put(uint32(len(name)))
	directory.WriteString(name)
	put(uint64(0))
	put(uint64(len(contents)))
	hash := md5.Sum(contents)
	directory.Write(hash[:])
	put(fileFlags)
	if version == 2 {
		// V2 puts the directory before the payload; align data after it.
		base := 96 + directory.Len()
		binary.LittleEndian.PutUint64(header[24:], uint64(base))
		return append(append(header[:96], directory.Bytes()...), contents...)
	}
	binary.LittleEndian.PutUint64(header[32:], uint64(128+len(contents)))
	return append(append(header, contents...), directory.Bytes()...)
}

func hasAuditIssue(in auditInspection, code string) bool {
	for _, issue := range in.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestAuditPCKVersionsAndFailClosed(t *testing.T) {
	for _, version := range []uint32{2, 3, 4} {
		b := auditTestPCK(version, 2, 0, "res://mods/example/main.gd", []byte("extends Node\n"))
		in := inspectAuditBytes(b)
		if len(in.Issues) != 0 || len(in.Files) != 2 || in.Files[1].Kind != "text" {
			t.Fatal(version, in)
		}
		if err := validateAuditInspection(in); err != nil {
			t.Fatal(err)
		}
		// Truncation must always yield incomplete, never panic or silently pass.
		for i := 0; i < len(b); i++ {
			inspectAuditBytes(b[:i])
		}
		b[len(b)-1] ^= 128
		if len(inspectAuditBytes(b).Issues) == 0 {
			t.Fatal("corrupt PCK passed")
		}
	}
	for _, test := range []struct {
		flags, fileFlags uint32
		name, code       string
	}{
		{3, 0, "res://main.gd", "encrypted"}, {2, 1, "res://main.gd", "encrypted"},
		{2, 4, "res://main.gd", "unsupported"}, {6, 0, "res://main.gd", "unsupported"},
		{2, 0, "res://../outside.gd", "unsafe_path"},
	} {
		in := inspectAuditBytes(auditTestPCK(3, test.flags, test.fileFlags, test.name, []byte("extends Node\n")))
		if !hasAuditIssue(in, test.code) {
			t.Fatal(test, in)
		}
	}
}

func TestAuditZIPIdentityLimitsAndAliases(t *testing.T) {
	b := auditTestZip(t, []string{"src/main.gd", "copy/main.gd"}, []string{"extends Node\n", "extends Node\n"})
	in := inspectAuditBytes(b)
	if len(in.Issues) != 0 || in.SHA256 != auditHash(b) {
		t.Fatal(in)
	}
	aliases := 0
	for _, f := range in.Files {
		if f.Kind == "text_alias" {
			aliases++
		}
	}
	if aliases != 1 {
		t.Fatal("duplicate source not reused", in.Files)
	}
	if err := validateAuditInspection(in); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/absolute", "C:/windows", "a/../../b", "a\\b"} {
		if !hasAuditIssue(inspectAuditBytes(auditTestZip(t, []string{name}, []string{"data"})), "unsafe_path") {
			t.Fatal(name)
		}
	}
	if !hasAuditIssue(inspectAuditBytes(auditTestZip(t, []string{"same", "same"}, []string{"a", "b"})), "duplicate_path") {
		t.Fatal("duplicate path passed")
	}
	bomb := auditTestZip(t, []string{"bomb"}, []string{strings.Repeat("a", 2<<20)})
	if !hasAuditIssue(inspectAuditBytes(bomb), "limit") {
		t.Fatal("compression bomb passed")
	}
	large := auditTestZip(t, []string{"large.gd"}, []string{strings.Repeat("x", auditMaxText+1)})
	if !hasAuditIssue(inspectAuditBytes(large), "limit") {
		t.Fatal("large source passed")
	}
	// Flip encryption flags in both ZIP headers; nothing gets decrypted/opened.
	for i := 0; i+10 < len(b); i++ {
		if string(b[i:i+4]) == "PK\x03\x04" {
			b[i+6] |= 1
		}
		if string(b[i:i+4]) == "PK\x01\x02" {
			b[i+8] |= 1
		}
	}
	if !hasAuditIssue(inspectAuditBytes(b), "encrypted") {
		t.Fatal("encrypted ZIP passed")
	}
}

func TestAuditOpaqueResourcesAndData(t *testing.T) {
	if !auditNumericJSON([]byte("[1,[2,3],null,true]")) || auditNumericJSON([]byte(`{"script":"execute"}`)) || auditNumericJSON([]byte(`["hidden instructions"]`)) {
		t.Fatal("numeric data classification")
	}
	b := auditTestZip(t, []string{"native.dll", "fake.png", "script.gde", "compiled.gdc"}, []string{"MZ\x00binary", "MZ\x00binary", "encrypted", "GDSC\x00data"})
	in := inspectAuditBytes(b)
	if !hasAuditIssue(in, "encrypted") || !hasAuditIssue(in, "unsupported") {
		t.Fatal(in)
	}
	for _, f := range in.Files {
		if f.Kind == "asset" {
			t.Fatal("fake asset accepted")
		}
	}
}

func auditTestResult(in auditInspection, c auditContext) auditModelResult {
	r := auditModelResult{Conclusion: "no_findings", Summary: "No concerning behavior in the supplied text.", Findings: []auditFinding{}, Metadata: auditMetadata{Name: c.Listing.Name, ShortDescription: c.Listing.ShortDescription, Description: c.Listing.Description, Aliases: []string{}, Evidence: []auditEvidence{}}}
	for _, f := range in.Files {
		if f.Kind == "text" {
			r.Reviewed = append(r.Reviewed, auditCoverage{f.Path, f.SHA256})
		}
	}
	return r
}

func TestAuditModelCoverageAndEvidence(t *testing.T) {
	in := inspectAuditBytes([]byte("extends Node\n# input\n"))
	c := auditContext{Listing: modInput{Name: "Manual name", ShortDescription: "Manual description"}}
	r := auditTestResult(in, c)
	if err := validateAuditResult(in, c, r); err != nil {
		t.Fatal(err)
	}
	r.Reviewed = nil
	if err := validateAuditResult(in, c, r); err == nil {
		t.Fatal("omitted source passed")
	}
	r = auditTestResult(in, c)
	r.Conclusion = "findings"
	r.Findings = []auditFinding{{Severity: "low", Category: "prompt_injection", Title: "Example", Evidence: []auditEvidence{{Path: "package", Line: 2, Quote: "# input"}}}}
	if err := validateAuditResult(in, c, r); err != nil {
		t.Fatal(err)
	}
	r.Findings[0].Evidence[0].Quote = "invented"
	if err := validateAuditResult(in, c, r); err == nil {
		t.Fatal("invented evidence passed")
	}
	r = auditTestResult(in, c)
	r.Metadata.Name = "Override"
	if err := validateAuditResult(in, c, r); err == nil {
		t.Fatal("unsupported editorial change passed")
	}
}

func TestAuditEvidenceUsesExactCitedLineAndAcceptsCRLF(t *testing.T) {
	text := "extends Node\r\n\tlog(\"diagnostic\")\r\n\t\tfile.seek_end()\r\n\t\tfile.store_line(text)\r\n{\"description\":\"Choose a local song.\"}\r\n"
	for _, tc := range []struct {
		name  string
		line  int
		quote string
		valid bool
	}{
		{"indent omitted", 2, "log(\"diagnostic\")", true},
		{"line endings only", 3, "file.seek_end()\n\t\tfile.store_line(text)", true},
		{"original line endings", 3, "\t\tfile.seek_end()\r\n\t\tfile.store_line(text)", true},
		{"within JSON value", 5, "Choose a local song.", true},
		{"wrong line", 1, "log(\"diagnostic\")", false},
		{"later line", 2, "file.seek_end()", false},
		{"different word", 5, "Choose a remote song.", false},
		{"altered whitespace", 2, "log( \"diagnostic\" )", false},
		{"altered indentation", 3, "file.seek_end()\n\tfile.store_line(text)", false},
		{"skipped line", 2, "log(\"diagnostic\")\n\t\tfile.store_line(text)", false},
		{"empty", 2, "", false},
		{"blank prefix", 2, "\n\t\tfile.seek_end()", false},
		{"zero line", 0, "extends Node", false},
		{"out of range", 100, "extends Node", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditEvidenceMatches(text, auditEvidence{Line: tc.line, Quote: tc.quote}); got != tc.valid {
				t.Fatalf("matched=%v want=%v", got, tc.valid)
			}
		})
	}
}

func TestAuditOfflineRevalidationPreservesEvidenceAndPartialCoverage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", filepath.Join(dir, "must-not-create"))
	t.Setenv("OPENAI_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	in := inspectAuditBytes([]byte("\tlog(\"diagnostic\")\r\n"))
	c := auditContext{ModID: 3, Listing: modInput{Name: "Manual title", ShortDescription: "Manual correction"}}
	result := auditTestResult(in, c)
	result.Conclusion = "findings"
	result.Findings = []auditFinding{{Severity: "low", Category: "suspicious_behavior", Title: "Local logging", Evidence: []auditEvidence{{Path: "package", Line: 1, Quote: "log(\"diagnostic\")"}}}}
	saved := auditReport{Policy: auditPolicy, SHA256: in.SHA256, Context: c, ContextSHA256: auditContextHash(c), State: "incomplete", Error: "finding quote does not match source line", CreatedAt: 123, MetadataCurrent: true}
	inputs := map[string]any{"inspection.json": in, "report.json": saved, "model-output.json": result}
	original := map[string][]byte{}
	for name, data := range inputs {
		if err := auditWriteJSON(filepath.Join(dir, name), data); err != nil {
			t.Fatal(err)
		}
		original[name], _ = os.ReadFile(filepath.Join(dir, name))
	}
	args := []string{"revalidate", "--inspection", filepath.Join(dir, "inspection.json"), "--report", filepath.Join(dir, "report.json"), "--model-output", filepath.Join(dir, "model-output.json"), "--out", filepath.Join(dir, "verified.json")}
	var output bytes.Buffer
	if err := runAudit(context.Background(), args, &output); err != nil {
		t.Fatal(err)
	}
	var verified auditReport
	if err := auditReadJSON(filepath.Join(dir, "verified.json"), &verified); err != nil {
		t.Fatal(err)
	}
	if verified.State != "findings" || verified.Error != "" || verified.CreatedAt != 123 || verified.MetadataCurrent || verified.ValidationVersion != auditValidationVersion || !strings.Contains(output.String(), `"model_calls":0`) {
		t.Fatal(verified, output.String())
	}
	for name, before := range original {
		after, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(before, after) {
			t.Fatalf("changed input %s", name)
		}
	}
	if _, err := os.Stat(os.Getenv("DATA_DIR")); !os.IsNotExist(err) {
		t.Fatal("opened a database during offline validation", err)
	}
	// A metadata quote fix cannot convert opaque binaries into a full review.
	in.Issues = []auditIssue{{Path: "package!/scene.scn", Code: "unsupported", Detail: "Opaque Godot scene"}}
	result = auditTestResult(in, c)
	partial, err := revalidateAudit(in, saved, &result)
	if err != nil || partial.State != "incomplete" || partial.Result == nil {
		t.Fatal(partial, err)
	}
	in.Issues[0].Code = "encrypted"
	blocked, err := revalidateAudit(in, saved, &result)
	if err != nil || blocked.State != "red_flag" || blocked.Result != nil {
		t.Fatal(blocked, err)
	}
	in.SHA256 = strings.Repeat("0", 64)
	if _, err = revalidateAudit(in, saved, &result); err == nil {
		t.Fatal("wrong artifact accepted")
	}
	in.SHA256 = saved.SHA256
	saved.Context.Listing.Name = "Edited without updating fingerprint"
	if _, err = revalidateAudit(in, saved, &result); err == nil {
		t.Fatal("changed context accepted")
	}
	args[len(args)-1] = filepath.Join(dir, "report.json")
	if err := runAudit(context.Background(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("overwrote the original report")
	}
}

func TestAuditDeduplicatesPaidCallsAndRespectsSharedBudget(t *testing.T) {
	a := testApp(t)
	in := inspectAuditBytes([]byte("extends Node\n"))
	c := auditContext{Listing: modInput{Name: "Operator name", ShortDescription: "Current manual correction"}}
	r := auditTestResult(in, c)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-6.2-astra" || body["reasoning"].(map[string]any)["effort"] != "high" || body["tools"] != nil || body["store"] != false || body["service_tier"] != "default" {
			t.Fatal(body)
		}
		if !strings.Contains(body["input"].(string), c.Listing.ShortDescription) {
			t.Fatal("manual context missing")
		}
		b, _ := json.Marshal(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(b)}}}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}})
	}))
	defer server.Close()
	client := &modelClient{app: a, key: "test", model: "gpt-6.2-astra", budget: 20e6, endpoint: server.URL, client: server.Client(), reasoning: "high", maxOutput: 12000}
	cache := t.TempDir()
	first, err := reviewAudit(context.Background(), client, cache, in, c)
	if err != nil || first.State != "no_findings" || first.Cached || calls != 1 {
		t.Fatal(first, err, calls)
	}
	second, err := reviewAudit(context.Background(), client, cache, in, c)
	if err != nil || !second.Cached || !second.MetadataCurrent || calls != 1 {
		t.Fatal(second, err, calls)
	}
	c.Listing.ShortDescription = "Intervening manual edit"
	third, err := reviewAudit(context.Background(), client, cache, in, c)
	if err != nil || !third.Cached || third.MetadataCurrent || calls != 1 {
		t.Fatal(third, err, calls)
	}
	if _, err = a.db.Exec("INSERT INTO ingest_usage(model,stage,reserved_micros,actual_micros,created_at) VALUES('test','existing',19999000,19999000,1)"); err != nil {
		t.Fatal(err)
	}
	other := inspectAuditBytes([]byte("extends Node\n# different\n"))
	report, err := reviewAudit(context.Background(), client, cache, other, c)
	if err == nil || report.State != "incomplete" || calls != 1 {
		t.Fatal("shared cap failed", report, err, calls)
	}
}

func TestAuditEncryptedNeverCallsModelAndCrashNeverRetries(t *testing.T) {
	in := inspectAuditBytes(auditTestPCK(3, 3, 0, "res://main.gd", []byte("extends Node")))
	cache := t.TempDir()
	r, err := reviewAudit(context.Background(), nil, cache, in, auditContext{})
	if err != nil || r.State != "red_flag" || r.Result != nil {
		t.Fatal(r, err)
	}
	in = inspectAuditBytes([]byte("extends Node\n"))
	dir := filepath.Join(cache, in.SHA256)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempt.json"), []byte("interrupted request"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewAudit(context.Background(), nil, cache, in, auditContext{}); err == nil {
		t.Fatal("interrupted request retried")
	}
}

func TestAuditCannotCreateLedgerOrRaiseCap(t *testing.T) {
	dir := t.TempDir()
	if db, err := openAuditLedger(dir); err == nil {
		db.Close()
		t.Fatal("created an audit database")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.db")); !os.IsNotExist(err) {
		t.Fatal("unexpected database file", err)
	}
	for _, cap := range []string{"21", "NaN", "+Inf", "0", "-1"} {
		args := []string{"review", "--inspection", "missing", "--mod", "1", "--out", "missing", "--budget", cap}
		if err := runAudit(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatal(cap, err)
		}
	}
}

func FuzzAuditPCKBounds(f *testing.F) {
	f.Add(auditTestPCK(3, 2, 0, "res://main.gd", []byte("extends Node\n")))
	f.Add([]byte("GDPC"))
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	entry, _ := w.Create("script.gd")
	_, _ = entry.Write([]byte("extends Node\n"))
	_ = w.Close()
	f.Add(archive.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 1<<20 {
			in := inspectAuditBytes(b)
			if in.SHA256 != fmt.Sprintf("%x", sha256.Sum256(b)) {
				t.Fatal("identity changed")
			}
		}
	})
}
