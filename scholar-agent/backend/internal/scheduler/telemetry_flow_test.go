package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scholar-agent-backend/internal/models"
	"scholar-agent-backend/internal/store"
	"scholar-agent-backend/internal/telemetry"

	openaiModel "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
)

// This exercises the production Scheduler -> Eino OpenAI client -> HTTP path.
// It uses no model key, external collector, Docker, Redis or paid service.
func TestSchedulerTelemetryWithRealModelClient(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retry       bool
		emptyChoice bool
		usage       string
	}{
		{"retry-after-http-error", true, false, `,"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}`},
		{"http-200-model-error", false, true, ""},
		{"unknown-usage", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type receivedSpan struct {
				TraceID, SpanID, ParentSpanID, Name string
				Attributes                          []struct {
					Key   string
					Value map[string]any
				}
				Status struct{ Code int }
			}
			var mu sync.Mutex
			var spans []receivedSpan
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "private-prompt") || strings.Contains(string(body), "private-answer") {
					t.Error("trace contains model content")
				}
				var envelope struct {
					ResourceSpans []struct {
						ScopeSpans []struct {
							Spans []receivedSpan
						}
					}
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Error(err)
				}
				mu.Lock()
				for _, resource := range envelope.ResourceSpans {
					for _, scope := range resource.ScopeSpans {
						spans = append(spans, scope.Spans...)
					}
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer sink.Close()
			exporter, err := telemetry.New(telemetry.Config{Enabled: true, Endpoint: sink.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = exporter.Close(ctx)
			}()
			var requests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if tc.retry && attempt == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"message":"private-provider-error","type":"server_error"}}`)
					return
				}
				if tc.emptyChoice {
					_, _ = io.WriteString(w, `{"id":"fixture","choices":[]}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"private-answer"},"finish_reason":"stop"}]`+tc.usage+`}`)
			}))
			defer provider.Close()
			model, err := openaiModel.NewChatModel(context.Background(), &openaiModel.ChatModelConfig{
				BaseURL: provider.URL, APIKey: "fixture-key", Model: "fixture", HTTPClient: exporter.LLMHTTPClient("fixture"),
			})
			if err != nil {
				t.Fatal(err)
			}
			planStore := store.NewMemoryPlanStore()
			plan := governanceTestPlan("telemetry-"+tc.name, 1)
			if tc.retry {
				plan.Nodes[0].RetryLimit = 1
			}
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			runner := NewScheduler(planStore, taskExecutorFunc(func(ctx context.Context, _ *models.PlanGraph, _ *models.TaskNode) (*models.TaskExecutionResult, error) {
				message, err := model.Generate(ctx, []*schema.Message{{Role: schema.User, Content: "private-prompt"}})
				if err != nil {
					return nil, err
				}
				return &models.TaskExecutionResult{Status: models.StatusCompleted, Result: message.Content}, nil
			}), &NoopEventPublisher{}, 1)
			if err := runner.ExecutePlan(telemetry.WithExporter(context.Background(), exporter), plan.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := exporter.Close(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			capturedSpans := append([]receivedSpan(nil), spans...)
			mu.Unlock()
			wantAttempts := 1
			if tc.retry {
				wantAttempts = 2
			}
			if len(capturedSpans) != 2*wantAttempts {
				t.Fatalf("expected one task and one LLM span per attempt: got %d", len(capturedSpans))
			}
			attrs := func(span receivedSpan) map[string]any {
				result := map[string]any{}
				for _, attr := range span.Attributes {
					for _, value := range attr.Value {
						result[attr.Key] = value
					}
				}
				return result
			}
			parents := map[string]receivedSpan{}
			executionIDs := map[any]bool{}
			for _, span := range capturedSpans {
				if span.Name == "task.execute" {
					parents[span.SpanID] = span
					executionIDs[attrs(span)["langfuse.observation.metadata.execution_id"]] = true
				}
			}
			if len(executionIDs) != wantAttempts || executionIDs[nil] {
				t.Fatal("task attempts lost distinct execution IDs")
			}
			for _, span := range capturedSpans {
				if span.Name != "llm.http" {
					continue
				}
				parent, ok := parents[span.ParentSpanID]
				if !ok || parent.TraceID != span.TraceID || attrs(parent)["langfuse.observation.metadata.execution_id"] != attrs(span)["langfuse.observation.metadata.execution_id"] {
					t.Fatal("real Eino request lost the execution context")
				}
				if tc.emptyChoice && (span.Status.Code != 1 || parent.Status.Code != 2) {
					t.Fatal("HTTP 200 must remain distinct from failed model/task semantics")
				}
				if tc.usage == "" && attrs(span)["gen_ai.usage.input_tokens"] != nil {
					t.Fatal("missing provider usage was incorrectly reported as zero")
				}
			}
		})
	}
}
