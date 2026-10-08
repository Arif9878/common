package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
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

	// Service, Environment and Version are added to every record.
	// Empty values are omitted.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

// Option configures [New] and [NewHandler].
type Option func(*options)

type options struct {
	writer     io.Writer
	levelVar   *slog.LevelVar
	redactKeys []string
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

// New returns a logger built from cfg. It returns an error if cfg.Level or
// cfg.Format is invalid.
func New(cfg Config, opts ...Option) (*slog.Logger, error) {
	o := options{writer: os.Stdout}
	for _, opt := range opts {
		opt(&o)
	}

	var level slog.Level
	if cfg.Level != "" {
		if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
			return nil, fmt.Errorf("logging: invalid level %q", cfg.Level)
		}
	}
	var leveler slog.Leveler = level
	if o.levelVar != nil {
		o.levelVar.Set(level)
		leveler = o.levelVar
	}

	hopts := &slog.HandlerOptions{Level: leveler, AddSource: cfg.AddSource}
	var base slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "", "json":
		base = slog.NewJSONHandler(o.writer, hopts)
	case "text":
		base = slog.NewTextHandler(o.writer, hopts)
	default:
		return nil, fmt.Errorf("logging: invalid format %q (want json or text)", cfg.Format)
	}

	var static []slog.Attr
	for _, a := range []slog.Attr{
		slog.String(KeyService, cfg.Service),
		slog.String(KeyEnvironment, cfg.Environment),
		slog.String(KeyVersion, cfg.Version),
	} {
		if a.Value.String() != "" {
			static = append(static, a)
		}
	}

	h := newHandler(base, o.redactKeys)
	if len(static) > 0 {
		return slog.New(h.WithAttrs(static)), nil
	}
	return slog.New(h), nil
}
