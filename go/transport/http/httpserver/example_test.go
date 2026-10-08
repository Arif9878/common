package httpserver_test

import (
	"context"
	"log"
	"log/slog"
	"net/http"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func Example() {
	logger := slog.Default()
	shutdown := graceful.New()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		order, err := loadOrder(r.Context(), r.PathValue("id"))
		if err != nil {
			httpserver.WriteError(w, r, err) // 404 problem+json for errors.NotFound
			return
		}
		_, _ = w.Write(order)
	})

	srv := httpserver.NewServer(httpserver.Config{Addr: ":8080"}, httpserver.Handler(mux, httpserver.WithLogger(logger)), logger)
	if err := httpserver.Serve(shutdown, srv); err != nil {
		log.Fatal(err)
	}
	if err := shutdown.Wait(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func loadOrder(context.Context, string) ([]byte, error) {
	return nil, errors.NotFound.New("order not found")
}
