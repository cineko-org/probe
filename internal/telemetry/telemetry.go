package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"unicode"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// SafeDiagnosticKey carries only the Go error class returned by
	// SafeDiagnostic. It intentionally avoids sensitive-key fragments.
	SafeDiagnosticKey = "provider_error_summary"
)

type Setup struct {
	Logger   *slog.Logger
	Shutdown func(context.Context) error
}

func New(ctx context.Context, serviceName string, output io.Writer) (Setup, error) {
	stdout := slog.NewJSONHandler(output, nil)
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return Setup{Logger: slog.New(newCanonicalHandler(stdout)), Shutdown: func(context.Context) error { return nil }}, nil
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return Setup{}, err
	}
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return Setup{}, err
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(provider)
	otlp := otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(provider))
	return Setup{Logger: slog.New(newCanonicalHandler(newFanoutHandler(stdout, otlp))), Shutdown: provider.Shutdown}, nil
}

func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	valueType := reflect.TypeOf(err)
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	if valueType.Name() == "" {
		return "reported_error"
	}
	return normalizeKey(valueType.Name())
}

// SafeDiagnostic deliberately never reads err.Error(). Provider and browser
// errors can contain arbitrary URLs, headers, userinfo, credentials, or
// tokens, so a deny-list cannot establish that structured logs are safe. The
// concrete Go error class is bounded to an identifier-derived value and keeps
// diagnostics useful without retaining attacker- or provider-controlled text.
func SafeDiagnostic(err error) string {
	return ErrorType(err)
}

type fanoutHandler struct{ handlers []slog.Handler }

func newFanoutHandler(handlers ...slog.Handler) slog.Handler {
	return &fanoutHandler{handlers: handlers}
}
func (handler *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, child := range handler.handlers {
		if child.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (handler *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var result error
	for _, child := range handler.handlers {
		if child.Enabled(ctx, record.Level) {
			result = errors.Join(result, child.Handle(ctx, record.Clone()))
		}
	}
	return result
}
func (handler *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, len(handler.handlers))
	for index, child := range handler.handlers {
		children[index] = child.WithAttrs(attrs)
	}
	return newFanoutHandler(children...)
}
func (handler *fanoutHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, len(handler.handlers))
	for index, child := range handler.handlers {
		children[index] = child.WithGroup(name)
	}
	return newFanoutHandler(children...)
}

type canonicalHandler struct{ next slog.Handler }

func newCanonicalHandler(next slog.Handler) slog.Handler { return &canonicalHandler{next: next} }
func (handler *canonicalHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}
func (handler *canonicalHandler) Handle(ctx context.Context, record slog.Record) error {
	normalized := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		if attribute, ok := normalizeAttribute(attribute); ok {
			normalized.AddAttrs(attribute)
		}
		return true
	})
	span := trace.SpanFromContext(ctx).SpanContext()
	if span.IsValid() {
		normalized.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	return handler.next.Handle(ctx, normalized)
}
func (handler *canonicalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	normalized := make([]slog.Attr, 0, len(attrs))
	for _, attribute := range attrs {
		if attribute, ok := normalizeAttribute(attribute); ok {
			normalized = append(normalized, attribute)
		}
	}
	return newCanonicalHandler(handler.next.WithAttrs(normalized))
}
func (handler *canonicalHandler) WithGroup(name string) slog.Handler {
	return newCanonicalHandler(handler.next.WithGroup(normalizeKey(name)))
}

func normalizeAttribute(attribute slog.Attr) (slog.Attr, bool) {
	attribute.Value = attribute.Value.Resolve()
	key := normalizeKey(attribute.Key)
	if key == "" || forbiddenKey(key) {
		return slog.Attr{}, false
	}
	if key == "error" || key == "err" {
		if err, ok := attribute.Value.Any().(error); ok {
			return slog.String("error_type", ErrorType(err)), true
		}
		return slog.String("error_type", "reported_error"), true
	}
	if attribute.Value.Kind() == slog.KindAny {
		return slog.Attr{}, false
	}
	attribute.Key = key
	return attribute, true
}

func forbiddenKey(key string) bool {
	for _, fragment := range []string{"authorization", "cookie", "credential", "password", "secret", "token", "user_id", "url"} {
		if key == fragment || strings.HasSuffix(key, "_"+fragment) || strings.HasPrefix(key, fragment+"_") {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	var output strings.Builder
	var previous rune
	lastUnderscore := false
	for index, current := range strings.TrimSpace(key) {
		switch {
		case unicode.IsLetter(current) || unicode.IsDigit(current):
			if unicode.IsUpper(current) && index > 0 && !lastUnderscore && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
				output.WriteByte('_')
			}
			output.WriteRune(unicode.ToLower(current))
			lastUnderscore = false
		case output.Len() > 0 && !lastUnderscore:
			output.WriteByte('_')
			lastUnderscore = true
		}
		previous = current
	}
	return strings.Trim(output.String(), "_")
}
