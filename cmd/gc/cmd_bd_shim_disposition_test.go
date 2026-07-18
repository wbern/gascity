package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// dispositionLogExporter captures emitted OTel log records so a test can assert
// the bd-shim disposition telemetry fired with the expected verb+disposition.
type dispositionLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func installDispositionLogExporter(t *testing.T) *dispositionLogExporter {
	t.Helper()
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(telemetry.ResetInstrumentsForTest)
	exp := &dispositionLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := otellogglobal.GetLoggerProvider()
	otellogglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otellogglobal.SetLoggerProvider(prev)
	})
	return exp
}

func (e *dispositionLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range records {
		e.records = append(e.records, rec.Clone())
	}
	return nil
}

func (e *dispositionLogExporter) Shutdown(context.Context) error   { return nil }
func (e *dispositionLogExporter) ForceFlush(context.Context) error { return nil }

// dispositionFor returns the disposition attribute of the first bd.shim.call
// record whose verb matches, or "" if none was emitted.
func (e *dispositionLogExporter) dispositionFor(verb string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.records {
		if e.records[i].Body().AsString() != "bd.shim.call" {
			continue
		}
		var gotVerb, gotDisp string
		e.records[i].WalkAttributes(func(kv otellog.KeyValue) bool {
			switch kv.Key {
			case "verb":
				gotVerb = kv.Value.AsString()
			case "disposition":
				gotDisp = kv.Value.AsString()
			}
			return true
		})
		if gotVerb == verb {
			return gotDisp
		}
	}
	return ""
}

// TestDispatchBdShimVerbViaAPIRecordsRouteDisposition proves that serving a verb
// through the controller records a "route" disposition — the signal that a
// direct worker->Dolt dial was avoided, which is how each bd-shim routing
// slice's connection cut becomes measurable.
func TestDispatchBdShimVerbViaAPIRecordsRouteDisposition(t *testing.T) {
	exp := installDispositionLogExporter(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gcg-1", "title": "t"}) //nolint:errcheck
	}))
	defer ts.Close()
	client := api.NewCityScopedClient(ts.URL, "alpha")

	var out, errb bytes.Buffer
	if code := dispatchBdShimVerbViaAPI(client, "create", []string{"t", "--type", "task"}, &out, &errb); code != 0 {
		t.Fatalf("create via API: code=%d err=%s", code, errb.String())
	}
	if got := exp.dispositionFor("create"); got != "route" {
		t.Fatalf("dispatchBdShimVerbViaAPI disposition = %q, want route", got)
	}
}
