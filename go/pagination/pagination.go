// Package pagination implements cursor (keyset) pagination for list
// endpoints: the client sends ?limit=…&cursor=…, the service queries the
// rows after the cursor's position, and the response carries the next
// cursor until the last page.
//
//	type orderKey struct {
//		CreatedAt time.Time `json:"t"`
//		ID        string    `json:"id"`
//	}
//	var cursors = pagination.NewCursor[orderKey]()
//
//	func listOrders(w http.ResponseWriter, r *http.Request) {
//		p, err := pagination.FromRequest(r)
//		after, err := cursors.Decode(p.Cursor) // zero orderKey on the first page
//		// SELECT … WHERE (created_at, id) < ($1, $2) ORDER BY created_at DESC, id DESC LIMIT p.Limit+1
//		orders, err := repo.List(ctx, after, p.Limit+1)
//		page, err := pagination.NewPage(orders, p.Limit, cursors, func(o Order) orderKey {
//			return orderKey{o.CreatedAt, o.ID}
//		})
//		_ = json.NewEncoder(w).Encode(page) // {"items": […], "next_cursor": "…"}
//	}
//
// Unlike offsets, cursors stay correct while rows are inserted or deleted,
// and every page costs the same, however deep: the query seeks the index
// to the cursor instead of skipping rows.
//
// Cursors are opaque to clients (base64url-encoded JSON of the sort key).
// [WithSigningKey] adds an HMAC so clients cannot forge them; without it a
// client can craft any position, which is harmless when the query applies
// the same authorization to every page.
//
// Invalid limits and cursors are errors of kind InvalidArgument naming the
// parameter (errors.FieldError), so httpserver answers 400 with the field
// in the problem document, like the validation package. For gRPC, map
// page_size and page_token to [Params] with [Validate].
package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Arif9878/common/go/errors"
)

// Params are a page request.
type Params struct {
	// Limit is the page size, between 1 and the maximum.
	Limit int
	// Cursor is the position to continue after; "" for the first page.
	Cursor string
}

// Option configures [FromQuery], [FromRequest] and [Validate].
type Option func(*options)

type options struct {
	defaultLimit, maxLimit int
	limitParam, cursorName string
}

// WithDefaultLimit sets the limit used when the request has none. The
// default is 20.
func WithDefaultLimit(n int) Option { return func(o *options) { o.defaultLimit = n } }

// WithMaxLimit sets the largest accepted limit. The default is 100.
func WithMaxLimit(n int) Option { return func(o *options) { o.maxLimit = n } }

// WithParamNames sets the query parameter (and field error) names. The
// defaults are "limit" and "cursor"; gRPC services might use "page_size"
// and "page_token".
func WithParamNames(limit, cursor string) Option {
	return func(o *options) { o.limitParam, o.cursorName = limit, cursor }
}

func newOptions(opts []Option) options {
	o := options{defaultLimit: 20, maxLimit: 100, limitParam: "limit", cursorName: "cursor"}
	for _, opt := range opts {
		opt(&o)
	}
	o.maxLimit = max(o.maxLimit, 1)
	o.defaultLimit = min(max(o.defaultLimit, 1), o.maxLimit)
	return o
}

// FromRequest reads the page parameters from r's query string.
func FromRequest(r *http.Request, opts ...Option) (Params, error) {
	return FromQuery(r.URL.Query(), opts...)
}

// FromQuery reads the page parameters from q, such as Echo's
// c.QueryParams().
func FromQuery(q url.Values, opts ...Option) (Params, error) {
	o := newOptions(opts)
	p := Params{Cursor: q.Get(o.cursorName)}
	if s := q.Get(o.limitParam); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return Params{}, invalid(o.limitParam, "type", "must be a whole number")
		}
		p.Limit = n
	}
	return validate(p, o)
}

// Validate applies the defaults and bounds to p, for parameters that come
// from elsewhere, such as a gRPC request's page_size and page_token: a
// Limit of 0 becomes the default limit.
func Validate(p Params, opts ...Option) (Params, error) {
	return validate(p, newOptions(opts))
}

func validate(p Params, o options) (Params, error) {
	switch {
	case p.Limit == 0:
		p.Limit = o.defaultLimit
	case p.Limit < 0:
		return Params{}, invalid(o.limitParam, "min", "must be at least 1")
	case p.Limit > o.maxLimit:
		return Params{}, invalid(o.limitParam, "max", "must be at most "+strconv.Itoa(o.maxLimit))
	}
	return p, nil
}

func invalid(field, rule, msg string) error {
	err := errors.WithPublicMessage(errors.InvalidArgument.New("pagination: invalid "+field), "the request is invalid")
	return errors.WithFields(err, errors.FieldError{Field: field, Rule: rule, Message: msg})
}

// Cursor encodes and decodes cursors holding a sort key of type K, a
// struct with the columns the list is ordered by. Its zero value encodes
// unsigned cursors.
type Cursor[K any] struct {
	key   []byte
	field string
}

// CursorOption configures [NewCursor].
type CursorOption func(*cursorOptions)

type cursorOptions struct {
	key   []byte
	field string
}

// WithSigningKey signs cursors with HMAC-SHA256 under key, so a cursor the
// service did not issue is rejected. Rotating the key invalidates the
// cursors clients hold; they start again from the first page.
func WithSigningKey(key []byte) CursorOption { return func(o *cursorOptions) { o.key = key } }

// WithCursorParam sets the parameter name used in errors. The default is
// "cursor".
func WithCursorParam(name string) CursorOption { return func(o *cursorOptions) { o.field = name } }

// NewCursor returns a cursor codec for sort keys of type K.
func NewCursor[K any](opts ...CursorOption) Cursor[K] {
	o := cursorOptions{field: "cursor"}
	for _, opt := range opts {
		opt(&o)
	}
	return Cursor[K](o)
}

// Encode returns the cursor for k.
func (c Cursor[K]) Encode(k K) (string, error) {
	b, err := json.Marshal(k)
	if err != nil {
		return "", errors.Internal.Wrap(err, "pagination: encode cursor")
	}
	if c.key != nil {
		b = append(b, c.sign(b)...)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Decode returns the sort key in s, or the zero K for "" (the first page).
// A malformed, tampered or foreign cursor is an InvalidArgument error
// naming the cursor parameter.
func (c Cursor[K]) Decode(s string) (K, error) {
	var k K
	if s == "" {
		return k, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return k, c.invalid()
	}
	if c.key != nil {
		if len(b) < sha256.Size {
			return k, c.invalid()
		}
		body, mac := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
		if !hmac.Equal(mac, c.sign(body)) {
			return k, c.invalid()
		}
		b = body
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, c.invalid()
	}
	return k, nil
}

func (c Cursor[K]) sign(b []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write(b)
	return m.Sum(nil)
}

func (c Cursor[K]) invalid() error {
	field := c.field
	if field == "" {
		field = "cursor"
	}
	return invalid(field, "cursor", "is not a cursor this API issued; start again without it")
}

// Page is a page of a list response.
type Page[T any] struct {
	Items []T `json:"items"`
	// NextCursor continues after the last item; it is empty on the last
	// page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// NewPage builds the page from items, which the query fetched with a limit
// of limit+1: if there are more than limit items, there is a next page,
// whose cursor is the sort key of the last item kept. Items is never nil,
// so an empty page encodes as "items": [].
func NewPage[T, K any](items []T, limit int, c Cursor[K], key func(T) K) (Page[T], error) {
	if items == nil {
		items = []T{}
	}
	if limit < 1 || len(items) <= limit {
		return Page[T]{Items: items}, nil
	}
	items = items[:limit]
	next, err := c.Encode(key(items[limit-1]))
	if err != nil {
		return Page[T]{}, err
	}
	return Page[T]{Items: items, NextCursor: next}, nil
}
