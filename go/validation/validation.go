// Package validation checks request structs with go-playground/validator
// tags and reports every invalid field to the client, by its JSON name:
//
//	type CreateOrder struct {
//		CustomerID string `json:"customer_id" validate:"required"`
//		Items      []Item `json:"items" validate:"required,min=1,dive"`
//	}
//	type Item struct {
//		SKU      string `json:"sku" validate:"required"`
//		Quantity int    `json:"quantity" validate:"gte=1,lte=100"`
//	}
//
//	// net/http: decode and validate in one step.
//	var req CreateOrder
//	if err := validation.Decode(r, &req); err != nil {
//		httpserver.WriteError(w, r, err)
//		return
//	}
//
//	// Echo: e.Validator = validation.New(), then in handlers
//	if err := c.Bind(&req); err != nil { return err }
//	if err := c.Validate(&req); err != nil { return err }
//
// Failures are errors of kind InvalidArgument carrying errors.FieldError
// values, so httpserver answers 400 with a problem document listing them,
// and grpcstatus sends them as a BadRequest detail:
//
//	{"status": 400, "code": "invalid_argument", "detail": "the request is invalid",
//	 "errors": [{"field": "items[0].quantity", "rule": "gte", "message": "must be at least 1"}]}
package validation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/Arif9878/common/go/errors"
)

// Validator validates structs. It is safe for concurrent use; create one
// and share it. Its Validate method makes it an echo.Validator.
type Validator struct {
	v *validator.Validate
}

// New returns a Validator that names fields by their JSON names.
func New() *Validator {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			return ""
		case "":
			return f.Name
		}
		return name
	})
	return &Validator{v: v}
}

// RegisterValidation adds a custom validation tag, as validator.Validate
// does.
func (v *Validator) RegisterValidation(tag string, fn validator.Func) error {
	return v.v.RegisterValidation(tag, fn)
}

var std = New()

// Struct validates s with a shared Validator.
func Struct(s any) error { return std.Validate(s) }

var errInvalid = errors.InvalidArgument.New("validation: invalid request")

// Validate checks s's validate tags. It returns nil, or an InvalidArgument
// error listing every invalid field.
func (v *Validator) Validate(s any) error {
	err := v.v.Struct(s)
	if err == nil {
		return nil
	}
	ves, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		// Not a struct, or a bad tag: a programming error.
		return errors.Internal.Wrap(err, "validation")
	}
	fields := make([]errors.FieldError, 0, len(ves))
	for _, fe := range ves {
		fields = append(fields, errors.FieldError{Field: path(fe.Namespace()), Rule: fe.Tag(), Message: message(fe)})
	}
	return errors.WithFields(errors.WithPublicMessage(errInvalid, "the request is invalid"), fields...)
}

// path drops the top-level struct's name from a namespace such as
// "CreateOrder.items[0].quantity".
func path(ns string) string {
	_, rest, ok := strings.Cut(ns, ".")
	if !ok {
		return ns
	}
	return rest
}

func message(fe validator.FieldError) string {
	p := fe.Param()
	countable := fe.Kind() == reflect.String || fe.Kind() == reflect.Slice || fe.Kind() == reflect.Map || fe.Kind() == reflect.Array
	unit := ""
	if countable {
		unit = " characters"
		if fe.Kind() != reflect.String {
			unit = " items"
		}
	}
	switch fe.Tag() {
	case "required", "required_if", "required_unless", "required_with", "required_without":
		return "is required"
	case "min", "gte":
		if countable {
			return "must have at least " + p + unit
		}
		return "must be at least " + p
	case "max", "lte":
		if countable {
			return "must have at most " + p + unit
		}
		return "must be at most " + p
	case "gt":
		return "must be greater than " + p
	case "lt":
		return "must be less than " + p
	case "len":
		return "must have exactly " + p + unit
	case "eq":
		return "must be " + p
	case "ne":
		return "must not be " + p
	case "oneof":
		return "must be one of: " + strings.Join(strings.Fields(p), ", ")
	case "email":
		return "must be an email address"
	case "url", "http_url":
		return "must be a URL"
	case "uuid", "uuid4", "uuid7":
		return "must be a UUID"
	case "datetime":
		return "must be a date and time in the format " + p
	case "e164":
		return "must be a phone number in E.164 format"
	case "alphanum":
		return "must contain only letters and digits"
	case "iso3166_1_alpha2":
		return "must be a two-letter country code"
	case "iso4217":
		return "must be a currency code"
	}
	return "is invalid (" + fe.Tag() + ")"
}

// Decode reads r's JSON body into dst, rejecting unknown fields, and
// validates it. Malformed JSON, a value of the wrong type, an unknown field
// or an invalid field are InvalidArgument errors naming the field where
// possible. Bound the body size with httpserver.MaxBytes.
func Decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.WithPublicMessage(errors.InvalidArgument.New("validation: trailing data"), "the body must be a single JSON value")
	}
	return std.Validate(dst)
}

func decodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.WithPublicMessage(errors.InvalidArgument.New("validation: empty body"), "the request body is empty")
	}
	var field errors.FieldError
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		field = errors.FieldError{Field: bracketIndexes(te.Field), Rule: "type", Message: "must be a " + jsonType(te.Type)}
	} else if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		field = errors.FieldError{Field: strings.Trim(name, `"`), Rule: "unknown", Message: "is not a known field"}
	} else {
		return errors.WithPublicMessage(errors.InvalidArgument.Wrap(err, "validation: decode"), "the request body is not valid JSON")
	}
	return errors.WithFields(errors.WithPublicMessage(errors.InvalidArgument.Wrap(err, "validation: decode"), "the request is invalid"), field)
}

// bracketIndexes turns encoding/json's "items.0.quantity" into
// "items[0].quantity", the form the validator uses.
func bracketIndexes(p string) string {
	parts := strings.Split(p, ".")
	var b strings.Builder
	for i, part := range parts {
		if _, err := strconv.Atoi(part); err == nil && i > 0 {
			b.WriteString("[" + part + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

func jsonType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "whole number"
	}
	return fmt.Sprint(t)
}
