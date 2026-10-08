// Package http holds legacy HTTP helpers.
//
// Deprecated: use github.com/Arif9878/common/go/lifecycle/graceful for
// signal handling and github.com/Arif9878/common/go/transport/http/httpserver
// for servers. This package will be removed in v1.0.
package http

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"
)

func NewContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Info("context is canceled!")
				cancel()
				return
			}
		}
	}()

	return ctx
}
