package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Correlation contains only allowlisted runtime identifiers, never task text,
// user prompts, artifacts, authentication or arbitrary task metadata.
type Correlation struct {
	TraceID     string
	PlanID      string
	TaskID      string
	ExecutionID string
}

type contextKey struct{}
type exporterKey struct{}
type scope struct {
	traceID string
	spanID  string
	meta    Correlation
}

// WithExporter supports explicit lifecycle ownership (and isolated tests).
func WithExporter(ctx context.Context, e *Exporter) context.Context {
	return context.WithValue(ctx, exporterKey{}, e)
}

func fromContext(ctx context.Context) *Exporter {
	if e, ok := ctx.Value(exporterKey{}).(*Exporter); ok {
		return e
	}
	return Default()
}

type attribute struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

type wireSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	ParentSpanID      string      `json:"parentSpanId,omitempty"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []attribute `json:"attributes"`
	Status            struct {
		Code int `json:"code"`
	} `json:"status"`
}

type span struct {
	exporter *Exporter
	wire     wireSpan
	once     sync.Once
}

func stringAttr(key, value string) attribute {
	return attribute{key, map[string]any{"stringValue": value}}
}

func intAttr(key string, value int64) attribute {
	return attribute{key, map[string]any{"intValue": strconv.FormatInt(value, 10)}}
}

func boolAttr(key string, value bool) attribute {
	return attribute{key, map[string]any{"boolValue": value}}
}

func normalizedTraceID(id string) string {
	compact := strings.ReplaceAll(id, "-", "")
	if decoded, err := hex.DecodeString(compact); err == nil && len(decoded) == 16 && compact != strings.Repeat("0", 32) {
		return strings.ToLower(compact)
	}
	if id == "" {
		return randomID(16)
	}
	digest := sha256.Sum256([]byte("researchpilot:trace:" + id))
	return hex.EncodeToString(digest[:16])
}

func randomID(size int) string {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		// A failure of the OS random source must not fail the task. Dropping the
		// trace is safer than issuing invalid or colliding span identifiers.
		return ""
	}
	return hex.EncodeToString(data)
}

func correlationAttrs(meta Correlation) []attribute {
	attrs := []attribute{}
	values := []struct{ key, value string }{
		{"trace_id", meta.TraceID}, {"plan_id", meta.PlanID},
		{"task_id", meta.TaskID}, {"execution_id", meta.ExecutionID},
	}
	if meta.TaskID != "" {
		values = append(values, struct{ key, value string }{"event_span_id", "task_" + meta.TaskID})
	}
	for _, item := range values {
		if item.value != "" {
			value := item.value
			if len(value) > 256 {
				value = value[:256]
			}
			// v4 filtering is observation-based; copy to every child span.
			attrs = append(attrs, stringAttr("langfuse.observation.metadata."+item.key, value))
		}
	}
	return attrs
}

func (e *Exporter) start(ctx context.Context, name, kind string, meta Correlation) (context.Context, *span) {
	if e == nil {
		return ctx, nil
	}
	parent, _ := ctx.Value(contextKey{}).(scope)
	traceID := parent.traceID
	if traceID == "" {
		traceID = normalizedTraceID(meta.TraceID)
	}
	id := randomID(8)
	if id == "" || traceID == "" {
		e.dropped.Add(1)
		return ctx, nil
	}
	if meta == (Correlation{}) {
		meta = parent.meta
	}
	s := &span{exporter: e, wire: wireSpan{
		TraceID: traceID, SpanID: id, ParentSpanID: parent.spanID,
		Name: name, Kind: 1, StartTimeUnixNano: strconv.FormatInt(time.Now().UnixNano(), 10),
		Attributes: append(correlationAttrs(meta), stringAttr("langfuse.observation.type", kind)),
	}}
	if kind == "generation" {
		s.wire.Kind = 3 // SPAN_KIND_CLIENT
	}
	return context.WithValue(ctx, contextKey{}, scope{traceID, id, meta}), s
}

func (s *span) finish(failed bool, attrs ...attribute) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.wire.EndTimeUnixNano = strconv.FormatInt(time.Now().UnixNano(), 10)
		s.wire.Status.Code = 1
		if failed {
			s.wire.Status.Code = 2
		}
		s.wire.Attributes = append(s.wire.Attributes, attrs...)
		s.exporter.enqueue(s.wire)
	})
}

// StartTask creates one parent span per task attempt. It does not change the
// business event trace/span IDs or execution leases used for recovery fencing.
func StartTask(ctx context.Context, meta Correlation) (context.Context, func(string)) {
	e := fromContext(ctx)
	if e == nil {
		return ctx, func(string) {}
	}
	ctx, s := e.start(ctx, "task.execute", "agent", meta)
	return ctx, func(status string) {
		switch status {
		case "completed", "skipped", "failed", "canceled", "interrupted":
		default:
			status = "unknown"
		}
		s.finish(status != "completed" && status != "skipped", stringAttr("researchpilot.task.status", status))
	}
}
