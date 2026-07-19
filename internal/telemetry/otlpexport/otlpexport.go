// Package otlpexport wires the OTLP/HTTP metric and log exporters into the
// telemetry package. It is the ONLY package that imports the OpenTelemetry
// OTLP/HTTP exporters, which transitively pull google.golang.org/grpc (~160
// packages). Binaries that export telemetry — gc — import this package and call
// Register once at startup; record-only binaries (the bd shim, and anything
// that only links internal/telemetry for its Record* API) never import it and
// therefore never link grpc.
package otlpexport

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// Register installs the OTLP/HTTP exporter constructors into the telemetry
// package. Call it once, before telemetry.Init, in binaries that export
// telemetry. Idempotent.
func Register() {
	telemetry.SetExporterFactories(newMetricExporter, newLogExporter)
}

func newMetricExporter(ctx context.Context, endpointURL string) (sdkmetric.Exporter, error) {
	return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(endpointURL))
}

func newLogExporter(ctx context.Context, endpointURL string) (sdklog.Exporter, error) {
	return otlploghttp.New(ctx, otlploghttp.WithEndpointURL(endpointURL))
}
