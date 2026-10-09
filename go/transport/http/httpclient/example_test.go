package httpclient_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/Arif9878/common/go/transport/http/httpclient"
)

func Example() {
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer partner.Close()

	// One client per dependency, reused: pooling, tracing, metrics, and
	// retries of idempotent requests on transient failures.
	client := httpclient.New(httpclient.Config{}, httpclient.WithName("partner"), httpclient.WithRetry())

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, partner.URL+"/v1/status", nil)
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()
	fmt.Println(resp.StatusCode)
	// Output: 202
}
