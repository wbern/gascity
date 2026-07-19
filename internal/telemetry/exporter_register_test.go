package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// init registers the real OTLP/HTTP exporters for the telemetry test binary,
// mirroring what internal/telemetry/otlpexport.Register does in production. The
// exporters are split into that sub-package so the shipped bd shim never links
// grpc; the test binary linking them is irrelevant. Without this, the Init
// tests would hit the "no OTLP exporter registered" guard.
func init() {
	SetExporterFactories(
		func(ctx context.Context, url string) (sdkmetric.Exporter, error) {
			return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(url))
		},
		func(ctx context.Context, url string) (sdklog.Exporter, error) {
			return otlploghttp.New(ctx, otlploghttp.WithEndpointURL(url))
		},
	)
}
