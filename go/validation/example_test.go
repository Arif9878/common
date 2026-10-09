package validation_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/httpserver"
	"github.com/Arif9878/common/go/validation"
)

type CreateOrder struct {
	CustomerID string `json:"customer_id" validate:"required"`
	Items      []Item `json:"items" validate:"required,min=1,dive"`
}

type Item struct {
	SKU      string `json:"sku" validate:"required"`
	Quantity int    `json:"quantity" validate:"gte=1,lte=100"`
}

func ExampleStruct() {
	err := validation.Struct(CreateOrder{Items: []Item{{SKU: "a", Quantity: 0}}})
	fmt.Println(errors.KindOf(err))
	for _, f := range errors.Fields(err) {
		fmt.Printf("%s %s: %s\n", f.Field, f.Rule, f.Message)
	}
	// Output:
	// invalid_argument
	// customer_id required: is required
	// items[0].quantity gte: must be at least 1
}

func ExampleDecode() {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var req CreateOrder
		if err := validation.Decode(r, &req); err != nil {
			httpserver.WriteError(w, r, err) // 400 problem+json listing the fields
			return
		}
		w.WriteHeader(http.StatusCreated)
	}

	// "coupon" is not a field of CreateOrder, so the request is rejected.
	body := `{"customer_id":"c-1","items":[{"sku":"a","quantity":1}],"coupon":"X"}`
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/orders", strings.NewReader(body)))
	fmt.Println(rec.Code)
	// Output: 400
}
