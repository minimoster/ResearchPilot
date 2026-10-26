// Package telemetry exports optional, metadata-only execution traces over
// OTLP/HTTP JSON. It is a small protocol client, not an OpenTelemetry SDK.
package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Enabled     bool
	Endpoint    string // Complete traces URL, including /v1/traces.
	PublicKey   string
	SecretKey   string
	QueueSize   int
	Timeout     time.Duration
	ServiceName string
}

// Stats counts spans, not requests. Failed exports are never reported as success.
type Stats struct {
	Enqueued uint64
	Exported uint64
	Dropped  uint64
	Failed   uint64
}

type Exporter struct {
	cfg       Config
	client    *http.Client
	queue     chan wireSpan
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
	enqueued  atomic.Uint64
	exported  atomic.Uint64
	dropped   atomic.Uint64
	failed    atomic.Uint64
}

// New starts no worker and validates no exporter settings while disabled.
func New(cfg Config) (*Exporter, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("telemetry: provide an HTTP(S) traces endpoint without URL credentials, query or fragment")
	}
	if (cfg.PublicKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("telemetry: both Langfuse keys must be set, or neither for a local collector")
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 256
	}
	if cfg.QueueSize < 1 || cfg.QueueSize > 4096 {
		return nil, errors.New("telemetry: queue size must be between 1 and 4096")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Timeout < time.Millisecond || cfg.Timeout > 10*time.Second {
		return nil, errors.New("telemetry: export timeout must be between 1ms and 10s")
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "ResearchPilot"
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Exporter{
		cfg: cfg, client: &http.Client{Timeout: cfg.Timeout,
			// Do not forward authentication to a redirected destination.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		queue: make(chan wireSpan, cfg.QueueSize), ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	go e.run()
	return e, nil
}

func ConfigFromEnv() (Config, error) {
	var cfg Config
	value := strings.TrimSpace(os.Getenv("RESEARCHPILOT_TRACE_ENABLED"))
	if value == "" || value == "false" || value == "0" {
		return cfg, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return cfg, errors.New("telemetry: invalid RESEARCHPILOT_TRACE_ENABLED")
	}
	cfg.Enabled = enabled
	if !enabled {
		return cfg, nil
	}
	cfg.Endpoint = strings.TrimSpace(os.Getenv("RESEARCHPILOT_OTLP_TRACES_ENDPOINT"))
	cfg.PublicKey = strings.TrimSpace(os.Getenv("LANGFUSE_PUBLIC_KEY"))
	cfg.SecretKey = strings.TrimSpace(os.Getenv("LANGFUSE_SECRET_KEY"))
	if value := os.Getenv("RESEARCHPILOT_TRACE_QUEUE_SIZE"); value != "" {
		cfg.QueueSize, err = strconv.Atoi(value)
		if err != nil {
			return cfg, errors.New("telemetry: invalid RESEARCHPILOT_TRACE_QUEUE_SIZE")
		}
	}
	if value := os.Getenv("RESEARCHPILOT_TRACE_TIMEOUT"); value != "" {
		cfg.Timeout, err = time.ParseDuration(value)
		if err != nil {
			return cfg, errors.New("telemetry: invalid RESEARCHPILOT_TRACE_TIMEOUT")
		}
	}
	return cfg, nil
}

var defaultOnce sync.Once
var defaultExporter *Exporter

// Default reads startup settings once. Configuration failures disable telemetry
// without failing the application. Error messages never contain config values.
func Default() *Exporter {
	defaultOnce.Do(func() {
		cfg, err := ConfigFromEnv()
		if err == nil {
			defaultExporter, err = New(cfg)
		}
		if err != nil {
			log.Printf("%v; tracing disabled", err)
		}
	})
	return defaultExporter
}

// Shutdown attempts a bounded drain on normal process exit. A hard process kill
// may lose queued spans; there is deliberately no durable outbox in this release.
func Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = Default().Close(ctx)
}

func (e *Exporter) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	return Stats{e.enqueued.Load(), e.exported.Load(), e.dropped.Load(), e.failed.Load()}
}

func (e *Exporter) enqueue(span wireSpan) {
	if e == nil {
		return
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- span:
		e.enqueued.Add(1)
	default:
		e.dropped.Add(1)
	}
}

// Close may race with span completion safely. It bounds draining by ctx and
// cancels in-flight exports on deadline, independently of application contexts.
func (e *Exporter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		close(e.queue)
		e.mu.Unlock()
	})
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		e.cancel()
		return ctx.Err()
	}
}

func (e *Exporter) run() {
	defer close(e.done)
	defer e.cancel()
	for span := range e.queue {
		if e.ctx.Err() != nil {
			e.dropped.Add(1)
			continue
		}
		if err := e.export(span); err != nil {
			count := e.failed.Add(1)
			// Rate-limit diagnostics, and never log request/response bodies or keys.
			if count == 1 || count%100 == 0 {
				log.Printf("telemetry: export failed (failed=%d, dropped=%d)", count, e.dropped.Load())
			}
		} else {
			e.exported.Add(1)
		}
	}
}

func (e *Exporter) export(span wireSpan) error {
	payload := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []attribute{stringAttr("service.name", e.cfg.ServiceName)}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]string{"name": "researchpilot/telemetry", "version": "1"},
			"spans": []wireSpan{span},
		}},
	}}}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-langfuse-ingestion-version", "4")
	if e.cfg.PublicKey != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(e.cfg.PublicKey+":"+e.cfg.SecretKey)))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxResponse = 64 << 10
	response, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(response) > maxResponse || resp.StatusCode != http.StatusOK {
		return errors.New("telemetry: export not acknowledged")
	}
	trimmed := bytes.TrimSpace(response)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("telemetry: invalid OTLP acknowledgement")
	}
	var ack struct {
		PartialSuccess *struct {
			RejectedSpans json.RawMessage `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(response, &ack); err != nil {
		return err
	}
	if ack.PartialSuccess != nil && len(ack.PartialSuccess.RejectedSpans) > 0 {
		raw := strings.Trim(string(ack.PartialSuccess.RejectedSpans), "\"")
		rejected, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || rejected > 0 {
			return fmt.Errorf("telemetry: span rejected in partial success")
		}
	}
	// One span per request; no retries, so partial-success batches cannot duplicate.
	return nil
}
