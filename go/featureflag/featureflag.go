// Package featureflag evaluates feature flags through a small interface, so
// business code does not depend on a flag provider.
//
//	type Deps struct{ Flags featureflag.Evaluator }
//
//	on, err := deps.Flags.Bool(ctx, "checkout.new-pricing", false,
//		featureflag.Attributes{featureflag.TargetingKey: userID, "country": country})
//	if err != nil {
//		logger.WarnContext(ctx, "flag evaluation failed; using default", logging.Err(err))
//	}
//	if on { ... }
//
// # Providers
//
// [NewOpenFeature] evaluates through OpenFeature
// (github.com/open-feature/go-sdk), which has providers for Flipt,
// LaunchDarkly, flagd, Unleash and others. The provider is registered with
// OpenFeature at startup; nothing provider-specific reaches this package:
//
//	openfeature.SetNamedProviderAndWait("orders", flipt.NewProvider(...))
//	flags := featureflag.NewOpenFeature(openfeature.NewClient("orders"))
//
// [Static] serves fixed values for tests and local development.
//
// # Failure behavior: choose the default deliberately
//
// Every evaluation returns a value, even when it fails (provider down,
// unknown flag, wrong type): then it returns defaultValue together with a
// classified error. The default therefore decides what happens during a
// flag outage, per call site:
//
//   - fail closed (default false) for risky features: a new payment flow
//     stays off when flags cannot be evaluated;
//   - fail open (default true) for kill switches of established behavior:
//     the existing path keeps working when flags cannot be evaluated.
//
// Errors are informational; most call sites log them and use the value.
//
// # Telemetry
//
// Each evaluation is counted in featureflag.evaluations (by flag and
// outcome: ok, default or error) and recorded as a feature_flag.evaluation
// event on the current span. Flag keys are metric labels, so keep them
// static strings. Attribute values are never recorded.
package featureflag

import (
	"context"
	"strconv"

	"github.com/open-feature/go-sdk/openfeature"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
)

// TargetingKey is the attribute naming the subject of an evaluation (user,
// tenant), used by providers for percentage rollouts.
const TargetingKey = "targetingKey"

// Attributes describe the subject of an evaluation for targeting rules.
type Attributes map[string]any

// Evaluator evaluates flags. Implementations return defaultValue with a
// classified error when evaluation fails.
type Evaluator interface {
	Bool(ctx context.Context, flag string, defaultValue bool, attrs Attributes) (bool, error)
	String(ctx context.Context, flag string, defaultValue string, attrs Attributes) (string, error)
	Int(ctx context.Context, flag string, defaultValue int64, attrs Attributes) (int64, error)
}

// Option configures [NewOpenFeature].
type Option func(*OpenFeature)

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(e *OpenFeature) { e.meterProv = mp }
}

// OpenFeature evaluates flags with an OpenFeature client.
type OpenFeature struct {
	client    *openfeature.Client
	meterProv metric.MeterProvider
	evals     metric.Int64Counter
}

var _ Evaluator = (*OpenFeature)(nil)

// NewOpenFeature returns an Evaluator using client.
func NewOpenFeature(client *openfeature.Client, opts ...Option) *OpenFeature {
	e := &OpenFeature{client: client, meterProv: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(e)
	}
	e.evals, _ = e.meterProv.Meter("github.com/Arif9878/common/go/featureflag").Int64Counter("featureflag.evaluations",
		metric.WithDescription("Flag evaluations by flag and outcome: ok, default (provider chose the default), error."))
	return e
}

func evalContext(attrs Attributes) openfeature.EvaluationContext {
	targeting, _ := attrs[TargetingKey].(string)
	rest := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if k != TargetingKey {
			rest[k] = v
		}
	}
	return openfeature.NewEvaluationContext(targeting, rest)
}

// Bool implements Evaluator.
func (e *OpenFeature) Bool(ctx context.Context, flag string, def bool, attrs Attributes) (bool, error) {
	d, err := e.client.BooleanValueDetails(ctx, flag, def, evalContext(attrs))
	return d.Value, e.done(ctx, flag, d.EvaluationDetails, strconv.FormatBool(d.Value), err)
}

// String implements Evaluator.
func (e *OpenFeature) String(ctx context.Context, flag string, def string, attrs Attributes) (string, error) {
	d, err := e.client.StringValueDetails(ctx, flag, def, evalContext(attrs))
	return d.Value, e.done(ctx, flag, d.EvaluationDetails, d.Variant, err)
}

// Int implements Evaluator.
func (e *OpenFeature) Int(ctx context.Context, flag string, def int64, attrs Attributes) (int64, error) {
	d, err := e.client.IntValueDetails(ctx, flag, def, evalContext(attrs))
	return d.Value, e.done(ctx, flag, d.EvaluationDetails, strconv.FormatInt(d.Value, 10), err)
}

func (e *OpenFeature) done(ctx context.Context, flag string, d openfeature.EvaluationDetails, value string, err error) error {
	outcome := "ok"
	if err != nil {
		outcome = "error"
		err = classify(flag, d.ErrorCode, err)
	} else if d.Reason == openfeature.DefaultReason {
		outcome = "default"
	}
	e.evals.Add(ctx, 1, metric.WithAttributes(attribute.String("flag", flag), attribute.String("outcome", outcome)))

	event := []attribute.KeyValue{attribute.String("feature_flag.key", flag)}
	if d.Variant != "" {
		event = append(event, attribute.String("feature_flag.result.variant", d.Variant))
	} else if err == nil {
		event = append(event, attribute.String("feature_flag.result.value", value))
	}
	if err != nil {
		event = append(event, attribute.String("error.type", errors.KindOf(err).String()))
	}
	trace.SpanFromContext(ctx).AddEvent("feature_flag.evaluation", trace.WithAttributes(event...))
	return err
}

func classify(flag string, code openfeature.ErrorCode, err error) error {
	msg := "featureflag " + flag
	switch code {
	case openfeature.FlagNotFoundCode:
		return errors.NotFound.Wrap(err, msg)
	case openfeature.TypeMismatchCode, openfeature.ParseErrorCode, openfeature.InvalidContextCode, openfeature.TargetingKeyMissingCode:
		return errors.InvalidArgument.Wrap(err, msg)
	case openfeature.ProviderNotReadyCode:
		return errors.Unavailable.Wrap(err, msg)
	default:
		return errors.Internal.Wrap(err, msg)
	}
}

// Static is an Evaluator with fixed values, for tests and local
// development. Unknown flags return the default with an error of kind
// NotFound; values of the wrong type return the default with
// InvalidArgument. Attributes are ignored.
type Static map[string]any

var _ Evaluator = Static(nil)

func staticValue[T any](s Static, flag string, def T) (T, error) {
	v, ok := s[flag]
	if !ok {
		return def, errors.NotFound.New("featureflag " + flag + ": not found")
	}
	t, ok := v.(T)
	if !ok {
		return def, errors.InvalidArgument.Errorf("featureflag %s: value is %T", flag, v)
	}
	return t, nil
}

// Bool implements Evaluator.
func (s Static) Bool(_ context.Context, flag string, def bool, _ Attributes) (bool, error) {
	return staticValue(s, flag, def)
}

// String implements Evaluator.
func (s Static) String(_ context.Context, flag string, def string, _ Attributes) (string, error) {
	return staticValue(s, flag, def)
}

// Int implements Evaluator. Values may be int or int64.
func (s Static) Int(_ context.Context, flag string, def int64, _ Attributes) (int64, error) {
	if v, ok := s[flag].(int); ok {
		return int64(v), nil
	}
	return staticValue(s, flag, def)
}
