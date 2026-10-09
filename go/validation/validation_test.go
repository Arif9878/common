package validation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/echoadapter"
	"github.com/Arif9878/common/go/transport/http/httpserver"
	"github.com/Arif9878/common/go/validation"
)

type item struct {
	SKU      string `json:"sku" validate:"required"`
	Quantity int    `json:"quantity" validate:"gte=1,lte=100"`
}

type createOrder struct {
	CustomerID string `json:"customer_id" validate:"required"`
	Email      string `json:"email" validate:"omitempty,email"`
	Currency   string `json:"currency" validate:"oneof=IDR USD"`
	Items      []item `json:"items" validate:"required,min=1,dive"`
	Internal   string `json:"-" validate:"max=3"`
}

func fieldsOf(err error) map[string]string {
	out := map[string]string{}
	for _, f := range errors.Fields(err) {
		out[f.Field] = f.Rule + ": " + f.Message
	}
	return out
}

func TestStruct(t *testing.T) {
	valid := createOrder{CustomerID: "c-1", Currency: "IDR", Items: []item{{SKU: "a", Quantity: 1}}}
	if err := validation.Struct(valid); err != nil {
		t.Fatalf("valid order: %v", err)
	}

	err := validation.Struct(createOrder{Email: "nope", Currency: "EUR", Items: []item{{Quantity: 0}, {SKU: "b", Quantity: 500}}})
	if errors.KindOf(err) != errors.InvalidArgument || errors.PublicMessage(err) != "the request is invalid" {
		t.Fatalf("err = %v", err)
	}
	want := map[string]string{
		"customer_id":       "required: is required",
		"email":             "email: must be an email address",
		"currency":          "oneof: must be one of: IDR, USD",
		"items[0].sku":      "required: is required",
		"items[0].quantity": "gte: must be at least 1",
		"items[1].quantity": "lte: must be at most 100",
	}
	got := fieldsOf(err)
	for f, w := range want {
		if got[f] != w {
			t.Errorf("%s: %q, want %q", f, got[f], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("fields = %v", got)
	}

	if err := validation.Struct(createOrder{CustomerID: "c", Currency: "IDR"}); fieldsOf(err)["items"] != "required: is required" {
		t.Errorf("missing items: %v", fieldsOf(err))
	}
	if err := validation.Struct(42); errors.KindOf(err) != errors.Internal {
		t.Errorf("non-struct: %v", err)
	}
}

func decode(t *testing.T, body string) error {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/orders", strings.NewReader(body))
	var req createOrder
	return validation.Decode(r, &req)
}

func TestDecode(t *testing.T) {
	if err := decode(t, `{"customer_id":"c","currency":"USD","items":[{"sku":"a","quantity":2}]}`); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	for name, tc := range map[string]struct{ body, field, rule, detail string }{
		"wrong type":      {`{"customer_id":1,"currency":"USD","items":[{"sku":"a","quantity":1}]}`, "customer_id", "type", ""},
		"wrong item type": {`{"customer_id":"c","currency":"USD","items":[{"sku":"a","quantity":"two"}]}`, "items[0].quantity", "type", ""},
		"unknown field":   {`{"customer_id":"c","price":1}`, "price", "unknown", ""},
		"invalid":         {`{"customer_id":"","currency":"USD","items":[{"sku":"a","quantity":1}]}`, "customer_id", "required", ""},
		"malformed":       {`{"customer_id":`, "", "", "the request body is not valid JSON"},
		"empty":           {``, "", "", "the request body is empty"},
		"two JSON value":  {`{"customer_id":"c","currency":"USD","items":[{"sku":"a","quantity":1}]} {}`, "", "", "the body must be a single JSON value"},
	} {
		err := decode(t, tc.body)
		if errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		if tc.detail != "" && errors.PublicMessage(err) != tc.detail {
			t.Errorf("%s: detail %q", name, errors.PublicMessage(err))
		}
		if tc.field != "" {
			f := errors.Fields(err)
			// Go 1.26's encoding/json leaves array indexes out of the path.
			if len(f) != 1 || (f[0].Field != tc.field && f[0].Field != strings.ReplaceAll(tc.field, "[0]", "")) || f[0].Rule != tc.rule {
				t.Errorf("%s: fields %+v, want %s/%s", name, f, tc.field, tc.rule)
			}
		}
	}
}

func TestEchoProblemResponse(t *testing.T) {
	e := echo.New()
	e.Validator = validation.New()
	e.HTTPErrorHandler = echoadapter.ErrorHandler
	e.Use(echoadapter.Middleware())
	e.POST("/orders", func(c echo.Context) error {
		var req createOrder
		if err := c.Bind(&req); err != nil {
			return err
		}
		if err := c.Validate(&req); err != nil {
			return err
		}
		return c.NoContent(http.StatusCreated)
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/orders",
		strings.NewReader(`{"currency":"USD","items":[{"sku":"a","quantity":0}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var p httpserver.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	fields := make([]string, 0, len(p.Errors))
	for _, f := range p.Errors {
		fields = append(fields, f.Field)
	}
	slices.Sort(fields)
	if rec.Code != http.StatusBadRequest || p.Code != "invalid_argument" || !slices.Equal(fields, []string{"customer_id", "items[0].quantity"}) {
		t.Errorf("%d %+v", rec.Code, p)
	}
}
