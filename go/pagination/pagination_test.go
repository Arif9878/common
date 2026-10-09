package pagination_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/pagination"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

type key struct {
	At time.Time `json:"t"`
	ID string    `json:"id"`
}

func fieldOf(t *testing.T, err error) errors.FieldError {
	t.Helper()
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	f := errors.Fields(err)
	if len(f) != 1 {
		t.Fatalf("fields = %+v", f)
	}
	return f[0]
}

func TestFromQuery(t *testing.T) {
	for q, want := range map[string]pagination.Params{
		"":                     {Limit: 20},
		"limit=5":              {Limit: 5},
		"limit=100&cursor=abc": {Limit: 100, Cursor: "abc"},
		"limit=0":              {Limit: 20},
	} {
		v, _ := url.ParseQuery(q)
		got, err := pagination.FromQuery(v)
		if err != nil || got != want {
			t.Errorf("FromQuery(%q) = %+v, %v; want %+v", q, got, err, want)
		}
	}
	for q, rule := range map[string]string{"limit=101": "max", "limit=-1": "min", "limit=ten": "type"} {
		v, _ := url.ParseQuery(q)
		_, err := pagination.FromQuery(v)
		if f := fieldOf(t, err); f.Field != "limit" || f.Rule != rule {
			t.Errorf("FromQuery(%q): field %+v, want limit/%s", q, f, rule)
		}
	}
}

func TestOptions(t *testing.T) {
	v := url.Values{"page_size": {"60"}}
	opts := []pagination.Option{pagination.WithParamNames("page_size", "page_token"), pagination.WithMaxLimit(50), pagination.WithDefaultLimit(10)}
	if _, err := pagination.FromQuery(v, opts...); fieldOf(t, err).Field != "page_size" {
		t.Errorf("error names %v", errors.Fields(err))
	}
	p, err := pagination.Validate(pagination.Params{Cursor: "x"}, opts...)
	if err != nil || p.Limit != 10 || p.Cursor != "x" {
		t.Errorf("Validate = %+v, %v", p, err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	k := key{At: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), ID: "o-9"}
	for name, c := range map[string]pagination.Cursor[key]{
		"unsigned": pagination.NewCursor[key](),
		"signed":   pagination.NewCursor[key](pagination.WithSigningKey([]byte("secret"))),
		"zero":     {},
	} {
		s, err := c.Encode(k)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(s, "+/=") {
			t.Errorf("%s: cursor %q is not URL-safe", name, s)
		}
		got, err := c.Decode(s)
		if err != nil || !got.At.Equal(k.At) || got.ID != k.ID {
			t.Errorf("%s: Decode = %+v, %v", name, got, err)
		}
	}
	if got, err := pagination.NewCursor[key]().Decode(""); err != nil || got != (key{}) {
		t.Errorf("empty cursor: %+v, %v", got, err)
	}
}

func TestCursorRejects(t *testing.T) {
	signed := pagination.NewCursor[key](pagination.WithSigningKey([]byte("secret")))
	other := pagination.NewCursor[key](pagination.WithSigningKey([]byte("other")))
	good, _ := signed.Encode(key{ID: "o-1"})
	forged, _ := pagination.NewCursor[key]().Encode(key{ID: "o-999"})
	foreign, _ := other.Encode(key{ID: "o-1"})
	tampered := []byte(good)
	tampered[3] ^= 1

	for name, s := range map[string]string{
		"not base64":  "%%%",
		"not JSON":    "bm90IGpzb24",
		"forged":      forged,
		"foreign key": foreign,
		"tampered":    string(tampered),
		"short":       "YQ",
	} {
		_, err := signed.Decode(s)
		if f := fieldOf(t, err); f.Field != "cursor" {
			t.Errorf("%s: field %+v", name, f)
		}
	}
}

func TestNewPage(t *testing.T) {
	c := pagination.NewCursor[string]()
	id := func(s string) string { return s }

	page, err := pagination.NewPage([]string{"a", "b", "c"}, 2, c, id)
	if err != nil || !slices.Equal(page.Items, []string{"a", "b"}) || page.NextCursor == "" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if after, _ := c.Decode(page.NextCursor); after != "b" {
		t.Errorf("next cursor decodes to %q, want the last item kept", after)
	}
	last, _ := pagination.NewPage([]string{"c"}, 2, c, id)
	if last.NextCursor != "" {
		t.Errorf("last page has a next cursor")
	}
	empty, _ := pagination.NewPage[string](nil, 2, c, id)
	b, _ := json.Marshal(empty)
	if string(b) != `{"items":[]}` {
		t.Errorf("empty page JSON = %s", b)
	}
}

// TestProblemResponse checks the 400 a handler sends for a bad cursor.
func TestProblemResponse(t *testing.T) {
	c := pagination.NewCursor[key]()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := pagination.FromRequest(r)
		if err == nil {
			_, err = c.Decode(p.Cursor)
		}
		if err != nil {
			httpserver.WriteError(w, r, err)
		}
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/orders?cursor=junk!", nil))
	var problem struct {
		Status int                 `json:"status"`
		Errors []errors.FieldError `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if rec.Code != http.StatusBadRequest || len(problem.Errors) != 1 || problem.Errors[0].Field != "cursor" {
		t.Errorf("response %d %s", rec.Code, rec.Body)
	}
}

func FuzzDecode(f *testing.F) {
	signed := pagination.NewCursor[key](pagination.WithSigningKey([]byte("secret")))
	good, _ := signed.Encode(key{ID: "o-1"})
	f.Add(good)
	f.Add("")
	f.Add("e30")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := signed.Decode(s)
		if err != nil {
			if errors.KindOf(err) != errors.InvalidArgument {
				t.Fatalf("Decode(%q): %v", s, err)
			}
			return
		}
		// Only cursors the codec issued decode: re-encoding gives back an
		// equivalent cursor.
		again, err := signed.Encode(k)
		if err != nil {
			t.Fatal(err)
		}
		if k2, err := signed.Decode(again); err != nil || k2.ID != k.ID || !k2.At.Equal(k.At) {
			t.Fatalf("round trip of %q: %+v, %v", s, k2, err)
		}
	})
}
