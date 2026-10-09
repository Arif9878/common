package logging

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/Arif9878/common/go/observability/internal/resenv"
)

// Config configures a logger. The zero value logs JSON at info level to
// standard output. Environment variable names are relative; the service
// chooses the prefix, for example LOG_.
type Config struct {
	// Level is the minimum level: debug, info, warn or error, optionally
	// with an offset such as "info+2".
	Level string `env:"LEVEL" envDefault:"info"`
	// Format is "json" (production) or "text" (local development).
	Format string `env:"FORMAT" envDefault:"json"`
	// AddSource adds the source file and line of the log call.
	AddSource bool `env:"ADD_SOURCE"`

	// Output is where records go: "stdout" (the writer, by default
	// standard output, for a log shipper or kubectl logs), "otlp" (the
	// OpenTelemetry LoggerProvider given with WithLoggerProvider; see the
	// logs package) or "both".
	Output string `env:"OUTPUT" envDefault:"stdout"`

	// TraceIDKey and SpanIDKey name the trace fields written to standard
	// output. The defaults, trace_id and span_id, suit Grafana Loki and most
	// pipelines; use trace.id and span.id for New Relic's and Elastic's
	// logs-in-context. OTLP records carry the IDs natively instead.
	TraceIDKey string `env:"TRACE_ID_KEY" envDefault:"trace_id"`
	SpanIDKey  string `env:"SPAN_ID_KEY" envDefault:"span_id"`

	// Service, Environment and Version are added to every record. Empty
	// fields are taken from the standard OpenTelemetry variables, as the
	// tracing, metrics and logs packages do: OTEL_SERVICE_NAME, and
	// service.name, deployment.environment.name and service.version in
	// OTEL_RESOURCE_ATTRIBUTES. So one pair of variables, set in the
	// container or Pod spec, names the service in every signal. Values
	// still empty are omitted.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

// Outputs accepted in [Config].Output.
const (
	OutputStdout = "stdout"
	OutputOTLP   = "otlp"
	OutputBoth   = "both"
)

// Validate reports whether cfg is valid. [New] calls it.
func (cfg Config) Validate() error {
	if _, err := cfg.level(); err != nil {
		return err
	}
	switch strings.ToLower(cfg.Format) {
	case "", "json", "text":
	default:
		return errors.New("invalid format (want json or text)")
	}
	switch strings.ToLower(cfg.Output) {
	case "", OutputStdout, OutputOTLP, OutputBoth:
	default:
		return errors.New("invalid output (want stdout, otlp or both)")
	}
	return nil
}

func (cfg Config) stdout() bool {
	o := strings.ToLower(cfg.Output)
	return o == "" || o == OutputStdout || o == OutputBoth
}

func (cfg Config) otlp() bool {
	o := strings.ToLower(cfg.Output)
	return o == OutputOTLP || o == OutputBoth
}

func (cfg Config) level() (slog.Level, error) {
	var level slog.Level
	if cfg.Level == "" {
		return level, nil
	}
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return level, errors.New("invalid level (want debug, info, warn or error)")
	}
	return level, nil
}

// Option configures [New] and [NewHandler].
type Option func(*options)

type options struct {
	writer     io.Writer
	levelVar   *slog.LevelVar
	redactKeys []string
	provider   otellog.LoggerProvider
}

// WithLoggerProvider sends records to provider when Config.Output is
// "otlp" or "both". Use the provider from logs.Init. It is ignored by
// [NewHandler].
func WithLoggerProvider(provider otellog.LoggerProvider) Option {
	return func(o *options) { o.provider = provider }
}

// WithWriter sets the log destination. The default is os.Stdout.
// It is ignored by [NewHandler].
func WithWriter(w io.Writer) Option {
	return func(o *options) { o.writer = w }
}

// WithLevelVar makes the logger read its minimum level from v, so it can be
// changed at runtime. New sets v to the configured level.
// It is ignored by [NewHandler].
func WithLevelVar(v *slog.LevelVar) Option {
	return func(o *options) { o.levelVar = v }
}

// WithRedactKeys adds key patterns to the default redaction list. Patterns
// are matched like the defaults: case-insensitively, ignoring '_', '-' and
// '.', as a substring of the key.
func WithRedactKeys(patterns ...string) Option {
	return func(o *options) { o.redactKeys = append(o.redactKeys, patterns...) }
}

// New returns a logger built from cfg. It returns an error if cfg is
// invalid, or if cfg.Output includes "otlp" without [WithLoggerProvider].
func New(cfg Config, opts ...Option) (*slog.Logger, error) {
	o := options{writer: os.Stdout}
	for _, opt := range opts {
		opt(&o)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("logging: %w", err)
	}
	level, _ := cfg.level()
	var leveler slog.Leveler = level
	if o.levelVar != nil {
		o.levelVar.Set(level)
		leveler = o.levelVar
	}

	traceKey, spanKey := cfg.TraceIDKey, cfg.SpanIDKey
	if traceKey == "" {
		traceKey = KeyTraceID
	}
	if spanKey == "" {
		spanKey = KeySpanID
	}

	var outputs []slog.Handler
	if cfg.stdout() {
		hopts := &slog.HandlerOptions{Level: leveler, AddSource: cfg.AddSource}
		if strings.EqualFold(cfg.Format, "text") {
			outputs = append(outputs, slog.NewTextHandler(o.writer, hopts))
		} else {
			outputs = append(outputs, slog.NewJSONHandler(o.writer, hopts))
		}
	}
	if cfg.otlp() {
		if o.provider == nil {
			return nil, errors.New("logging: output " + cfg.Output + " needs WithLoggerProvider (see the logs package)")
		}
		otel := otelslog.NewHandler("github.com/Arif9878/common/go/observability/logging",
			otelslog.WithLoggerProvider(o.provider), otelslog.WithSource(cfg.AddSource))
		// OTLP records carry trace context natively; drop the text fields.
		outputs = append(outputs, &otlpOutput{next: otel, level: leveler, drop: []string{traceKey, spanKey}})
	}
	base := outputs[0]
	if len(outputs) > 1 {
		base = fanout(outputs)
	}

	id := resenv.Lookup()
	var static []slog.Attr
	for _, a := range []slog.Attr{
		slog.String(KeyService, cmp.Or(cfg.Service, id.Service)),
		slog.String(KeyEnvironment, cmp.Or(cfg.Environment, id.Environment)),
		slog.String(KeyVersion, cmp.Or(cfg.Version, id.Version)),
	} {
		if a.Value.String() != "" {
			static = append(static, a)
		}
	}

	h := newHandler(base, o.redactKeys)
	h.traceKey, h.spanKey = traceKey, spanKey
	if len(static) > 0 {
		return slog.New(h.WithAttrs(static)), nil
	}
	return slog.New(h), nil
}
