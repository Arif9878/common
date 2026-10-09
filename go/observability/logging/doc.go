// Package logging builds the organization's standard [log/slog] logger.
//
// There is no custom logger interface: packages and services accept a
// *slog.Logger. This package only standardizes how that logger is built:
//
//	logger, err := logging.New(cfg.Log) // cfg.Log is a logging.Config
//	if err != nil {
//		return err
//	}
//	logger.InfoContext(ctx, "order created", logging.Duration(time.Since(start)))
//
// Use the *Context logging methods (InfoContext, ErrorContext, ...) so the
// handler can read identifiers from the context.
//
// # What the handler adds
//
// The handler returned by [New] (or [NewHandler] around any slog.Handler)
// adds, for every record:
//
//   - trace_id and span_id from the OpenTelemetry span in the context
//   - request_id from [requestid.FromContext]
//   - attributes attached to the context with [ContextWithAttrs], such as
//     topic, partition and offset set by a Kafka consumer, tenant_id set by
//     the tenant package and user_id set by jwtauth
//
// These are always top-level fields, even when the logger has groups.
//
// # Redaction
//
// Attribute values are replaced by "[REDACTED]" when the attribute key looks
// sensitive. Keys are compared case-insensitively with '_', '-' and '.'
// removed, and redacted when they contain password, passwd, secret, token,
// apikey, authorization, cookie, privatekey or credential. This
// deliberately over-redacts: "tokens_used" is redacted too. Extra patterns
// can be added with [WithRedactKeys]. Redaction applies inside groups and to
// values produced by [slog.LogValuer].
//
// Redaction is key-based and cannot see inside free text. Never log request
// or message payloads, and do not format secrets into messages or error
// strings: an error returned by a driver may contain a connection string, so
// classify and log errors at boundaries you control.
//
// # Standard fields
//
// Use the Key* constants and the [Err] and [Duration] helpers instead of
// ad-hoc names so that dashboards and queries work across services. Field
// names are snake_case.
package logging
