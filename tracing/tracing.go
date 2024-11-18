package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// TODO: How we use OTEL needs an overhaul, as such some of these functions will likely go away, however that
//  day is not today. We should probably create a single span for each incoming request
//  and avoid child spans if possible. See https://jeremymorrell.dev/blog/a-practitioners-guide-to-wide-events/

// NewResource creates a resource with sensible defaults. Replaces common use case of verbose usage.
func NewResource(serviceName, version string, resources ...*resource.Resource) (*resource.Resource, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("error in resource.Merge: %w", err)
	}

	for i, res2 := range resources {
		res, err = resource.Merge(res, res2)
		if err != nil {
			return nil, fmt.Errorf("error in resource.Merge on resources index %d: %w", i, err)
		}
	}

	return res, nil
}

func StartScope(ctx context.Context, spanName string, opts ...trace.SpanStartOption) context.Context {
	fileTag := getFileTag(1)
	opts = append(opts, trace.WithAttributes(
		attribute.String("file", fileTag),
	))

	ctx, _ = Tracer().Start(ctx, spanName, opts...)
	return ctx
}

// EndScope end scope created by `StartScope()`/`StartScope()`.
// Logs error return value and ends span.
func EndScope(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)

	// If scope returns an error, mark span with error.
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	span.End()
}

// Tracer returns a tracer object.
func Tracer(opts ...trace.TracerOption) trace.Tracer {
	return otel.Tracer(globalLibraryName, opts...)
}

var globalLibraryName string

type ShutdownFunc func(ctx context.Context) error

// InitTracing initializes a global OpenTelemetry tracer provider singleton.
func InitTracing(ctx context.Context, log *slog.Logger, libraryName string, opts ...sdktrace.TracerProviderOption) (ShutdownFunc, error) {
	exporter, err := makeOtlpExporter(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("error in makeOtlpExporter: %w", err)
	}

	exportProcessor := sdktrace.NewBatchSpanProcessor(exporter)
	opts = append(opts, sdktrace.WithSpanProcessor(exportProcessor))

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	if libraryName == "" {
		libraryName = "github.com/gubernator-io/gubernator/v3"

	}
	globalLibraryName = libraryName

	// Required for trace propagation between services.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}, err
}

// or returns the first string which is not "", returns "" if all strings provided are ""
func or(names ...string) string {
	for _, name := range names {
		if name != "" {
			return name
		}
	}

	return ""
}

func makeOtlpExporter(ctx context.Context, log *slog.Logger) (*otlptrace.Exporter, error) {
	protocol := or(
		os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"),
		"grpc")
	var client otlptrace.Client

	switch protocol {
	case "grpc":
		client = otlptracegrpc.NewClient()
	case "http/protobuf":
		client = otlptracehttp.NewClient()
	default:
		log.Error("unknown OTLP exporter protocol", "OTEL_EXPORTER_OTLP_PROTOCOL", protocol)
		protocol = "grpc"
		client = otlptracegrpc.NewClient()
	}

	attrs := []slog.Attr{
		slog.String("exporter", "otlp"),
		slog.String("protocol", protocol),
		slog.String("endpoint", or(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
			os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))),
	}

	sampler := os.Getenv("OTEL_TRACES_SAMPLER")
	attrs = append(attrs, slog.String("sampler", sampler))
	if strings.HasSuffix(sampler, "traceidratio") {
		ratio, _ := strconv.ParseFloat(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 64)
		attrs = append(attrs, slog.Float64("sampler.ratio", ratio))
	}

	log.LogAttrs(ctx, slog.LevelInfo, "Initializing OpenTelemetry", attrs...)

	return otlptrace.New(ctx, client)
}

// getFileTag returns file name:line of the caller.
//
// Use skip=0 to get the caller of getFileTag.
//
// Use skip=1 to get the caller of the caller(a getFileTag() wrapper).
func getFileTag(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)

	// Determine source file and line number.
	if !ok {
		// Rare condition.  Probably a bug in caller.
		return "unknown"
	}

	return file + ":" + strconv.Itoa(line)
}
