// Package config loads typed configuration from environment variables and
// validates it at startup.
//
// Configuration is an explicit struct, loaded once in main and passed down.
// There is no global configuration and no access by string key:
//
//	type Config struct {
//		HTTPAddr string          `env:"HTTP_ADDR" envDefault:":8080"`
//		DBURL    config.Secret   `env:"DATABASE_URL,required"`
//		Timeout  time.Duration   `env:"TIMEOUT" envDefault:"5s" validate:"min=1ms"`
//		Log      logging.Config  `envPrefix:"LOG_"`
//		Tracing  tracing.Config  `envPrefix:"TRACING_"`
//	}
//
//	cfg, err := config.Load[Config](config.WithPrefix("ORDERS_"))
//	if err != nil {
//		log.Fatal(err) // fail fast: never start with invalid configuration
//	}
//	logger.Info("starting", "config", config.LogValue(cfg))
//
// # Tags
//
// Parsing uses github.com/caarlos0/env/v11; see its documentation for all
// tags. The common ones are env:"NAME" with the options required, notEmpty,
// file (read the value from the file the variable names, as used for
// mounted Kubernetes secrets) and unset; envDefault:"value"; and
// envPrefix:"PREFIX_" on nested structs. Durations use [time.ParseDuration].
//
// Platform packages export Config structs with relative names (LEVEL,
// ENDPOINT); the service picks the prefix for each.
//
// # Validation
//
// After parsing, [Load] runs [Validate]: validate:"..." struct tags
// (github.com/go-playground/validator/v10, for example min=1, oneof=a b,
// hostname_port, url), then the Validate() error method of every struct in
// the configuration that has one, outermost first. Nil pointers to nested
// structs are skipped; env only allocates them when tagged env:",init".
//
// Problems are reported together, in field order, so one failed start shows
// every mistake: first all parse errors (missing or malformed variables);
// if parsing succeeded, all validation errors.
//
// # Secrets
//
// Declare secret values as [Secret]. A Secret prints, marshals and logs as
// "[REDACTED]"; call [Secret.Reveal] where the value is used. Errors
// produced by this package never include configuration values, including
// parse errors. Validate methods should follow the same rule; those in the
// platform packages do.
package config

import (
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/go-playground/validator/v10"

	"github.com/Arif9878/common/go/errors"
)

// Option configures [Load].
type Option func(*options)

type options struct {
	prefix      string
	environment map[string]string
}

// WithPrefix prepends prefix to every variable name, for example "ORDERS_".
func WithPrefix(prefix string) Option {
	return func(o *options) { o.prefix = prefix }
}

// WithEnvironment makes Load read variables from env instead of the process
// environment. Use it in tests instead of setting process variables.
func WithEnvironment(env map[string]string) Option {
	return func(o *options) { o.environment = maps.Clone(env) }
}

// Validator is implemented by configuration structs that check themselves.
type Validator interface {
	Validate() error
}

// Load parses T from the environment and validates it. On failure it returns
// the zero T and an error describing every problem found.
func Load[T any](opts ...Option) (T, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	// A nil Environment makes env read the process environment.
	cfg, err := env.ParseAsWithOptions[T](env.Options{
		Prefix:      o.prefix,
		Environment: o.environment,
	})
	if err != nil {
		var zero T
		return zero, errors.InvalidArgument.Wrap(sanitize(err), "config")
	}
	if err := Validate(&cfg); err != nil {
		var zero T
		return zero, err
	}
	return cfg, nil
}

// Validate checks cfg, a struct or pointer to struct, as described in the
// package documentation. Load calls it; call it directly for configuration
// built in code, such as in tests.
func Validate(cfg any) error {
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return errors.InvalidArgument.Errorf("config: Validate needs a struct, got %T", cfg)
	}

	var errs []error
	errs = append(errs, validateTags(v)...)
	walkValidators(v, "", &errs)
	if len(errs) == 0 {
		return nil
	}
	return errors.InvalidArgument.Wrap(errors.Join(errs...), "config")
}

func validateTags(v reflect.Value) []error {
	err := validator.New(validator.WithRequiredStructEnabled()).Struct(v.Interface())
	if err == nil {
		return nil
	}
	fieldErrs, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return []error{err}
	}
	out := make([]error, 0, len(fieldErrs))
	for _, fe := range fieldErrs {
		// Namespace is "Root.Field.Sub"; drop the root type name. The value
		// is deliberately not included.
		path := fe.Namespace()
		if i := strings.IndexByte(path, '.'); i >= 0 {
			path = path[i+1:]
		}
		rule := fe.Tag()
		if fe.Param() != "" {
			rule += "=" + fe.Param()
		}
		out = append(out, fmt.Errorf("%s: must satisfy %s", path, rule))
	}
	return out
}

var validatorType = reflect.TypeFor[Validator]()

func walkValidators(v reflect.Value, path string, errs *[]error) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}

	var val Validator
	switch {
	case v.CanAddr() && v.Addr().Type().Implements(validatorType):
		val = v.Addr().Interface().(Validator)
	case v.Type().Implements(validatorType):
		val = v.Interface().(Validator)
	}
	if val != nil {
		if err := val.Validate(); err != nil {
			if path != "" {
				err = fmt.Errorf("%s: %w", path, err)
			}
			*errs = append(*errs, err)
		}
	}

	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if path != "" {
			name = path + "." + name
		}
		walkValidators(v.Field(i), name, errs)
	}
}

// sanitize rewrites parse errors from env so that they never contain the
// offending value, which may be a secret placed in the wrong variable.
func sanitize(err error) error {
	agg, ok := errors.AsType[env.AggregateError](err)
	if !ok {
		return sanitizeOne(err)
	}
	out := make([]error, len(agg.Errors))
	for i, e := range agg.Errors {
		out[i] = sanitizeOne(e)
	}
	return errors.Join(out...)
}

func sanitizeOne(err error) error {
	pe, ok := errors.AsType[env.ParseError](err)
	if !ok {
		return err
	}
	msg := fmt.Sprintf("field %s: invalid value for type %s", pe.Name, pe.Type)
	if ne, ok := errors.AsType[*strconv.NumError](pe.Err); ok {
		msg += ": " + ne.Err.Error() // "invalid syntax" or "value out of range"
	}
	return errors.New(msg)
}
