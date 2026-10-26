package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu    sync.Mutex
	raw   []string
	spans []wireSpan
}

func receiver(t *testing.T, code int, ack string) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("x-langfuse-ingestion-version") != "4" {
			t.Error("missing OTLP JSON / Langfuse v4 headers")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []wireSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Error(err)
		}
		c.mu.Lock()
		c.raw = append(c.raw, string(data))
		for _, resource := range payload.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				c.spans = append(c.spans, scope.Spans...)
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, ack)
	}))
	t.Cleanup(server.Close)
	return server, c
}

func closeExporter(t *testing.T, e *Exporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func values(s wireSpan) map[string]any {
	result := map[string]any{}
	for _, attr := range s.Attributes {
		for _, value := range attr.Value {
			result[attr.Key] = value
		}
	}
	return result
}

func TestDisabledPreservesOriginalObjectsAndIgnoresExporterSettings(t *testing.T) {
	e, err := New(Config{Endpoint: ":invalid", QueueSize: -1})
	if err != nil || e != nil {
		t.Fatalf("disabled exporter = %v, %v", e, err)
	}
	if e.WrapTransport(http.DefaultTransport, "model") != http.DefaultTransport || e.LLMHTTPClient("model") != nil {
		t.Fatal("disabled instrumentation changed the original HTTP path")
	}
	ctx := WithExporter(context.Background(), nil)
	got, finish := StartTask(ctx, Correlation{TraceID: "test"})
	finish("completed")
	if got != ctx {
		t.Fatal("disabled instrumentation changed the task context")
	}
	t.Setenv("RESEARCHPILOT_TRACE_ENABLED", "false")
	t.Setenv("RESEARCHPILOT_TRACE_QUEUE_SIZE", "not-an-int")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("disabled env config = %+v, %v", cfg, err)
	}
}

func TestHTTPUsagePresenceParentCorrelationAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, status string
		wantInput, wantOutput          any
	}{
		{"known-zero", `{"choices":[{"message":{"content":"private-answer"}}],"usage":{"prompt_tokens":0,"completion_tokens":4,"total_tokens":4}}`, "application/json", "provider_reported", "0", "4"},
		{"missing-usage", `{"choices":[{"message":{"content":"private-answer"}}]}`, "application/json", "not_reported", nil, nil},
		{"partial-usage", `{"usage":{"prompt_tokens":5}}`, "application/json", "provider_reported", "5", nil},
		{"truncated", `{"usage":{"prompt_tokens":5`, "application/json", "invalid_or_incomplete_json", nil, nil},
		{"non-json", `private-answer`, "text/plain", "not_json", nil, nil},
		{"stream", "data: {\"usage\":{\"prompt_tokens\":5}}\n\n", "text/event-stream", "streaming_not_collected", nil, nil},
		{"oversize", `{"content":"` + strings.Repeat("x", maxUsageResponseBytes) + `","usage":{"prompt_tokens":5}}`, "application/json", "response_too_large", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, received := receiver(t, http.StatusOK, `{}`)
			e, err := New(Config{Enabled: true, Endpoint: sink.URL})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeExporter(t, e) })
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("traceparent") != "" {
					t.Error("internal trace context leaked to provider")
				}
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer provider.Close()
			ctx, finish := StartTask(WithExporter(context.Background(), e), Correlation{
				TraceID: "trace-my-plan", PlanID: "plan-1", TaskID: "task-1", ExecutionID: "attempt-1",
			})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, strings.NewReader("private-prompt"))
			req.Header.Set("Authorization", "Bearer private-provider-key")
			resp, err := e.LLMHTTPClient("test-model").Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if err != nil || closeErr != nil || string(body) != tc.body {
				t.Fatalf("instrumentation changed response: %v / %v", err, closeErr)
			}
			finish("completed")
			finish("failed") // Final spans are exported once, never updated.
			closeExporter(t, e)
			if len(received.spans) != 2 || e.Stats().Exported != 2 {
				t.Fatalf("expected task + HTTP span: %#v", e.Stats())
			}
			var parent, child wireSpan
			for _, span := range received.spans {
				if span.Name == "task.execute" {
					parent = span
				} else {
					child = span
				}
				if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(span.TraceID) || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(span.SpanID) {
					t.Fatalf("invalid OTLP IDs: %+v", span)
				}
				if span.StartTimeUnixNano == "" || span.EndTimeUnixNano < span.StartTimeUnixNano {
					t.Fatal("missing or reversed timestamps")
				}
				if values(span)["langfuse.observation.metadata.execution_id"] != "attempt-1" {
					t.Fatal("execution metadata must appear on every span")
				}
			}
			if child.ParentSpanID != parent.SpanID || child.TraceID != parent.TraceID {
				t.Fatal("LLM span lost task parent context")
			}
			attrs := values(child)
			if attrs["researchpilot.usage.status"] != tc.status || attrs["gen_ai.usage.input_tokens"] != tc.wantInput || attrs["gen_ai.usage.output_tokens"] != tc.wantOutput {
				t.Fatalf("usage presence changed: %#v", attrs)
			}
			for _, raw := range received.raw {
				for _, secret := range []string{"private-prompt", "private-answer", "private-provider-key"} {
					if strings.Contains(raw, secret) {
						t.Fatalf("export leaked %q", secret)
					}
				}
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorBody struct {
	readErr, closeErr error
}

func (b *errorBody) Read(p []byte) (int, error) { return copy(p, "abc"), b.readErr }
func (b *errorBody) Close() error             { return b.closeErr }

func TestResponseReadAndCloseErrorsAreUnchanged(t *testing.T) {
	sink, received := receiver(t, http.StatusOK, `{}`)
	e, _ := New(Config{Enabled: true, Endpoint: sink.URL})
	readErr, closeErr := errors.New("private-read-error"), errors.New("private-close-error")
	base := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &errorBody{readErr, closeErr}}, nil
	})
	req, _ := http.NewRequest(http.MethodGet, "http://unused.invalid", nil)
	resp, err := e.WrapTransport(base, "test").RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 16)
	n, gotErr := resp.Body.Read(p)
	if n != 3 || string(p[:n]) != "abc" || gotErr != readErr || resp.Body.Close() != closeErr {
		t.Fatal("original body semantics changed")
	}
	closeExporter(t, e)
	if len(received.spans) != 1 || received.spans[0].Status.Code != 2 || strings.Contains(received.raw[0], "private-") {
		t.Fatal("failure span duplicated, lost, or leaked raw error text")
	}
}

func TestExporterRejectsFailedAndPartialAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		ack  string
	}{
		{"unauthorized", 401, `{"error":"private-auth-detail"}`},
		{"unavailable", 503, `{}`},
		{"partial", 200, `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"private"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, received := receiver(t, tc.code, tc.ack)
			e, _ := New(Config{Enabled: true, Endpoint: sink.URL})
			ctx, cancel := context.WithCancel(WithExporter(context.Background(), e))
			_, finish := StartTask(ctx, Correlation{})
			cancel() // Export must survive application cancellation.
			finish("canceled")
			closeExporter(t, e)
			if e.Stats().Failed != 1 || e.Stats().Exported != 0 || len(received.spans) != 1 {
				t.Fatalf("failure/partial ack incorrectly counted or retried: %+v", e.Stats())
			}
		})
	}
}

func TestQueueSaturationAndConcurrentShutdownDoNotBlockTask(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer sink.Close()
	defer close(release)
	e, _ := New(Config{Enabled: true, Endpoint: sink.URL, QueueSize: 1, Timeout: 10 * time.Second})
	ctx := WithExporter(context.Background(), e)
	_, first := StartTask(ctx, Correlation{})
	first("completed")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			_, end := StartTask(ctx, Correlation{})
			end("completed")
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("task completion blocked on exporter")
	}
	if e.Stats().Dropped == 0 {
		t.Fatal("bounded queue did not account for dropped spans")
	}
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown was not bounded: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, end := StartTask(ctx, Correlation{}); end("completed") }()
	}
	wg.Wait()
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after shutdown cancellation")
	}
}
