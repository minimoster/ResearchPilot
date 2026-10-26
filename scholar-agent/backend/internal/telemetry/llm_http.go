package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
)

const maxUsageResponseBytes = 1 << 20

// LLMHTTPClient returns nil when disabled, preserving Eino's original client.
// Only model clients opt in; ordinary tools and the exporter never use it.
func LLMHTTPClient(model string) *http.Client {
	return Default().LLMHTTPClient(model)
}

func (e *Exporter) LLMHTTPClient(model string) *http.Client {
	if e == nil {
		return nil
	}
	return &http.Client{Transport: e.WrapTransport(http.DefaultTransport, model)}
}

// WrapTransport preserves the caller's base transport when disabled. It never
// reads request bodies, injects trace headers, changes retries or modifies URLs.
func (e *Exporter) WrapTransport(base http.RoundTripper, model string) http.RoundTripper {
	if e == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if len(model) > 256 {
		model = model[:256]
	}
	return &llmTransport{base: base, exporter: e, model: model}
}

type llmTransport struct {
	base     http.RoundTripper
	exporter *Exporter
	model    string
}

func (t *llmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	_, s := t.exporter.start(req.Context(), "llm.http", "generation", Correlation{})
	attrs := []attribute{
		stringAttr("gen_ai.operation.name", "chat"),
		stringAttr("gen_ai.request.model", t.model),
		stringAttr("researchpilot.observation.boundary", "http_response"),
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		s.finish(true, append(attrs, stringAttr("error.type", errorClass(err)), stringAttr("researchpilot.usage.status", "unknown"))...)
		return resp, err
	}
	if resp == nil {
		s.finish(true, append(attrs, stringAttr("error.type", "nil_response"))...)
		return resp, nil
	}
	attrs = append(attrs, intAttr("http.response.status_code", int64(resp.StatusCode)))
	if resp.Body == nil {
		s.finish(resp.StatusCode >= 400, append(attrs, stringAttr("researchpilot.usage.status", "no_body"))...)
		return resp, nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	resp.Body = &usageBody{
		ReadCloser: resp.Body, span: s, attrs: attrs, status: resp.StatusCode,
		isJSON: mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"),
		streaming: mediaType == "text/event-stream",
	}
	return resp, nil
}

// usageBody is transparent: callers receive the exact bytes, n/error and Close
// error of the original body. A bounded private copy is discarded after parsing
// usage only. Neither that copy nor provider error text enters the export queue.
type usageBody struct {
	io.ReadCloser
	span      *span
	attrs     []attribute
	status    int
	isJSON    bool
	streaming bool
	mu        sync.Mutex
	done      bool
	tooLarge  bool
	sample    []byte
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if !b.done && b.isJSON && !b.tooLarge && n > 0 {
		if len(b.sample)+n > maxUsageResponseBytes {
			b.tooLarge = true
			b.sample = nil
		} else {
			b.sample = append(b.sample, p[:n]...)
		}
	}
	b.mu.Unlock()
	if err != nil {
		b.finish(err == io.EOF, err)
	}
	return n, err
}

func (b *usageBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(false, err)
	return err
}

func (b *usageBody) finish(eof bool, readErr error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	data, tooLarge := b.sample, b.tooLarge
	b.sample = nil
	b.mu.Unlock()

	attrs := append(b.attrs, boolAttr("researchpilot.body.eof", eof))
	failed := b.status >= 400
	if readErr != nil && readErr != io.EOF {
		failed = true
		attrs = append(attrs, stringAttr("error.type", errorClass(readErr)))
	} else if failed {
		attrs = append(attrs, stringAttr("error.type", "http_error"))
	}
	usageStatus := "unknown"
	switch {
	case b.streaming:
		usageStatus = "streaming_not_collected"
	case !b.isJSON:
		usageStatus = "not_json"
	case tooLarge:
		usageStatus = "response_too_large"
	case failed:
		usageStatus = "failed_response"
	default:
		// json.Decoder may stop at a complete JSON value before EOF; Close then
		// still has a complete, valid usage-bearing response. Truncated JSON is
		// never treated as zero tokens.
		usageAttrs, status := parseUsage(data)
		attrs = append(attrs, usageAttrs...)
		usageStatus = status
	}
	attrs = append(attrs, stringAttr("researchpilot.usage.status", usageStatus))
	b.span.finish(failed, attrs...)
}

func parseUsage(data []byte) ([]attribute, string) {
	var payload struct {
		Usage *struct {
			Input  *int64 `json:"prompt_tokens"`
			Output *int64 `json:"completion_tokens"`
			Total  *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, "invalid_or_incomplete_json"
	}
	if payload.Usage == nil {
		return nil, "not_reported"
	}
	attrs := []attribute{}
	for _, field := range []struct {
		key   string
		value *int64
	}{
		{"gen_ai.usage.input_tokens", payload.Usage.Input},
		{"gen_ai.usage.output_tokens", payload.Usage.Output},
		{"gen_ai.usage.total_tokens", payload.Usage.Total},
	} {
		if field.value != nil && *field.value >= 0 {
			attrs = append(attrs, intAttr(field.key, *field.value))
		}
	}
	if len(attrs) == 0 {
		return nil, "not_reported"
	}
	return attrs, "provider_reported"
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "network_timeout"
		}
		return "transport_or_body_error"
	}
}
