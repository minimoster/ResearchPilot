package agent

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"scholar-agent-backend/internal/models"
	"testing"
)

func TestRegressionRejectsNonFiniteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []benchmarkPredictionRecord
	}{
		{"infinite prediction", []benchmarkPredictionRecord{{Prediction: "Inf", Target: 0}}},
		{"infinite target", []benchmarkPredictionRecord{{Prediction: 0, Target: "-Inf"}}},
		{"nan prediction", []benchmarkPredictionRecord{{Prediction: "NaN", Target: 0}}},
		{"nan target", []benchmarkPredictionRecord{{Prediction: 0, Target: "NaN"}}},
		{"subtraction overflow", []benchmarkPredictionRecord{{Prediction: 1e308, Target: -1e308}}},
		{"square overflow", []benchmarkPredictionRecord{{Prediction: 1e200, Target: 0}}},
		{"sum overflow", []benchmarkPredictionRecord{{Prediction: 1e154, Target: 0}, {Prediction: 1e154, Target: 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A finite fabricated score must never pass because the tolerance became Inf.
			for _, metric := range []string{"mse", "mae"} {
				if err := validateBenchmarkMetricsAgainstPredictions(map[string]float64{metric: 0}, tc.records, "regression"); err == nil {
					t.Fatalf("accepted non-finite regression evidence for %s", metric)
				}
			}
		})
	}
}

func TestBenchmarkMetricCloseRejectsNonFinite(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if benchmarkMetricClose(0, value) || benchmarkMetricClose(value, 0) || benchmarkMetricClose(value, value) {
			t.Fatalf("accepted non-finite value %v", value)
		}
	}
	if !benchmarkMetricClose(1.00001, 1) || benchmarkMetricClose(1.1, 1) {
		t.Fatal("finite tolerance changed")
	}
}

func TestRegressionFiniteNumericStrings(t *testing.T) {
	records := []benchmarkPredictionRecord{{Prediction: "3", Target: "1"}, {Prediction: 2.0, Target: 3.0}}
	if err := validateBenchmarkMetricsAgainstPredictions(map[string]float64{"mse": 2.5, "mae": 1.5}, records, "regression"); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeBenchmarkMetricsRejectsAmbiguousNames(t *testing.T) {
	for _, raw := range []string{
		`{"accuracy":0,"accuracy":1}`,
		`{"accuracy":0," Accuracy ":1}`,
		`{"MSE":0,"mse":1}`,
	} {
		if _, err := decodeBenchmarkMetrics([]byte(raw)); err == nil {
			t.Errorf("accepted ambiguous metric names: %s", raw)
		}
	}
	metrics, err := decodeBenchmarkMetrics([]byte(`{" Accuracy ":0.5,"macro_f1":0.25}`))
	if err != nil {
		t.Fatal(err)
	}
	if score, ok := benchmarkMetric(metrics, "accuracy"); !ok || score != 0.5 {
		t.Fatal("unambiguous alias no longer supported")
	}
}

func TestBenchmarkMetricNamesUseConsistentCanonicalization(t *testing.T) {
	metrics, err := decodeBenchmarkMetrics([]byte(`{"mse":0,"mſe":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if value, ok := benchmarkMetric(metrics, "mse"); !ok || value != 0 {
			t.Fatal("Unicode alias shadowed mse")
		}
	}
	if err := validateBenchmarkMetricsAgainstPredictions(map[string]float64{"mſe": 0}, []benchmarkPredictionRecord{{Prediction: 0, Target: 0}}, "regression"); err == nil {
		t.Fatal("Unicode lookalike satisfied required metric")
	}
}

func TestBenchmarkMetricJSONStructure(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"":1}`, `{" ":1}`, `{"mse":null}`, `{"mse":true}`, `{"mse":"0"}`, `{"mse":1e999}`, `{"mse":0`, `{"mse":0} {}`, `{"mse":0,}`} {
		if _, err := decodeBenchmarkMetrics([]byte(raw)); err == nil {
			t.Errorf("accepted malformed metrics %s", raw)
		}
	}
}

func TestBenchmarkOutputRejectsInfiniteRegressionArtifacts(t *testing.T) {
	workspace, manifest, _, _ := benchmarkHarnessFixture(t)
	manifest.SuggestedTask = "regression"
	directory := filepath.Join(workspace, ".scholar", "benchmark", "run")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	runManifest, err := json.Marshal(models.BenchmarkRunManifest{Status: "ok", DatasetSHA256: manifest.SHA256, SampleCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"run_manifest.json": runManifest,
		"metrics.json":      []byte(`{"mse":0}`),
		"predictions.jsonl": []byte(`{"prediction":"Inf","target":0}`),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := validateBenchmarkOutputDirectory(workspace, ".scholar/benchmark/run", manifest, 1, "run"); err == nil || report.Status == "passed" {
		t.Fatalf("accepted infinite artifact: report=%+v err=%v", report, err)
	}
}
