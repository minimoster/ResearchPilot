package agent

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"scholar-agent-backend/internal/models"
	"strings"
	"testing"
)

func TestBenchmarkRejectsUnboundEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, predictions string
		count             int
		want              string
	}{
		{"fabricated targets", "{\"index\":0,\"prediction\":\"invented\",\"target\":\"invented\"}\n{\"index\":1,\"prediction\":\"invented\",\"target\":\"invented\"}\n", 2, "does not match uploaded dataset"},
		{"duplicate index", "{\"index\":0,\"prediction\":\"positive\",\"target\":\"positive\"}\n{\"index\":0,\"prediction\":\"negative\",\"target\":\"negative\"}\n", 2, "must identify dataset index 1"},
		{"missing index", "{\"prediction\":\"positive\",\"target\":\"positive\"}\n{\"prediction\":\"negative\",\"target\":\"negative\"}\n", 2, "must identify dataset index 0"},
		{"cherry picked sample", "{\"index\":0,\"prediction\":\"positive\",\"target\":\"positive\"}\n", 1, "violates limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, manifest, _, _ := benchmarkHarnessFixture(t)
			output := filepath.Join(workspace, ".scholar", "benchmark", "run")
			if err := os.MkdirAll(output, 0700); err != nil {
				t.Fatal(err)
			}
			run, _ := json.Marshal(models.BenchmarkRunManifest{Status: "ok", DatasetSHA256: manifest.SHA256, SampleCount: tc.count})
			for name, content := range map[string][]byte{"metrics.json": []byte(`{"accuracy":1}`), "run_manifest.json": run, "predictions.jsonl": []byte(tc.predictions)} {
				if err := os.WriteFile(filepath.Join(output, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := validateBenchmarkOutputDirectory(workspace, ".scholar/benchmark/run", manifest, 2, "run"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRegressionRejectsNonFiniteAndOverflow(t *testing.T) {
	for _, tc := range []benchmarkPredictionRecord{{Prediction: "Inf", Target: 0}, {Prediction: 0, Target: "-Inf"}, {Prediction: "NaN", Target: 0}, {Prediction: 1e308, Target: -1e308}} {
		if err := validateBenchmarkMetricsAgainstPredictions(map[string]float64{"mse": 0}, []benchmarkPredictionRecord{tc}, "regression"); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
	}
	if benchmarkMetricClose(0, math.Inf(1)) {
		t.Fatal("infinite tolerance accepted finite metric")
	}
}

func TestBenchmarkRejectsDuplicateNormalizedMetricNames(t *testing.T) {
	if _, err := decodeBenchmarkMetrics([]byte(`{"accuracy":1," Accuracy ":0}`)); err == nil {
		t.Fatal("accepted ambiguous aliases")
	}
}

func TestBenchmarkDatasetValidationReadsBeyondProfileSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.csv")
	data := "text,label\n" + strings.Repeat("text,label\n", 1001)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	rows, count, err := readBenchmarkRowsWithLimit(benchmarkUploadedFile{Name: "data.csv", StoragePath: path}, 1001)
	if err != nil || len(rows) != 1001 || count != 1001 {
		t.Fatalf("rows=%d count=%d err=%v", len(rows), count, err)
	}
}

func TestRestorePaperBackupRecreatesDeletedFile(t *testing.T) {
	workspace := t.TempDir()
	original := []byte("print('original')\n")
	backups := map[string]paperCodeBackup{"main.py": {Content: original, Mode: 0600}}
	if err := restorePaperCodeBackups(workspace, backups); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "main.py"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("not restored: %q %v", got, err)
	}
}

func TestRestorePaperBackupContinuesAfterUnsafePath(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "a")); err != nil {
		t.Fatal(err)
	}
	backups := map[string]paperCodeBackup{"a/escape.py": {Content: []byte("bad"), Mode: 0600}, "z.py": {Content: []byte("original"), Mode: 0600}}
	if err := restorePaperCodeBackups(workspace, backups); err == nil {
		t.Fatal("accepted symlink parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.py")); !os.IsNotExist(err) {
		t.Fatal("escaped workspace")
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "z.py")); err != nil || string(got) != "original" {
		t.Fatalf("safe backup skipped: %q %v", got, err)
	}
}

func TestPaperPatchRejectsStaleSource(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.py"), []byte("print('new')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := applyPaperCodePatches(workspace, []paperCodePatchProposal{{Path: "main.py", Content: "print('patch')", Reason: "test"}}, map[string]string{"main.py": "print('old')\n"}, map[string]paperCodeBackup{}, 1)
	if err == nil || !strings.Contains(err.Error(), "changed since repair context") {
		t.Fatalf("accepted stale context: %v", err)
	}
}

func TestBenchmarkPreservesLargeNumericClassificationLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "predictions.jsonl")
	if err := os.WriteFile(path, []byte("{\"index\":0,\"prediction\":9007199254740992,\"target\":9007199254740993}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := readBenchmarkPredictionLines(path, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	accuracy, _ := recomputeClassificationMetrics(records)
	if accuracy != 0 {
		t.Fatal("distinct integer labels were rounded into an apparent match")
	}
	dataset := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(dataset, []byte(`[{"text":"sample","label":9007199254740993}]`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := readJSONBenchmarkRows(dataset)
	if err != nil || rows[0]["label"] != "9007199254740993" {
		t.Fatalf("dataset label lost precision: %#v %v", rows, err)
	}
}
