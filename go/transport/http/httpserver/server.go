package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Arif9878/common/go/lifecycle/graceful"
)

// Config configures an *http.Server. Zero fields use the defaults shown.
// Environment variable names are relative; the service chooses the prefix,
// for example HTTP_.
type Config struct {
	// Addr is the listen address, host:port or :port.
	Addr string `env:"ADDR" envDefault:":8080"`
	// ReadHeaderTimeout bounds reading request headers; it protects against
	// slow-header (Slowloris) attacks.
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`
	// ReadTimeout bounds reading the whole request, including the body.
	ReadTimeout time.Duration `env:"READ_TIMEOUT" envDefault:"30s"`
	// WriteTimeout bounds the time from the end of the request headers to
	// the end of the response. Set it to a negative value to disable it
	// for streaming.
	WriteTimeout time.Duration `env:"WRITE_TIMEOUT" envDefault:"35s"`
	// IdleTimeout bounds how long keep-alive connections wait for the next
	// request.
	IdleTimeout time.Duration `env:"IDLE_TIMEOUT" envDefault:"120s"`
	// MaxHeaderBytes bounds the size of request headers.
	MaxHeaderBytes int `env:"MAX_HEADER_BYTES" envDefault:"1048576"`
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	def(&c.ReadHeaderTimeout, 5*time.Second)
	def(&c.ReadTimeout, 30*time.Second)
	def(&c.WriteTimeout, 35*time.Second)
	def(&c.IdleTimeout, 120*time.Second)
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = 1 << 20
	}
	return c
}

// NewServer returns an *http.Server for h with the timeouts from cfg. The
// default WriteTimeout (35s) is a little longer than the default request
// Timeout of [Handler] (30s), so handlers can still write a 504 response.
// net/http's own error log is routed to logger at warn level; nil uses
// slog.Default().
func NewServer(cfg Config, h http.Handler, logger *slog.Logger) *http.Server {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout < 0 {
		writeTimeout = 0
	}
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Serve listens on srv.Addr and serves in the background under g: the
// server's Shutdown is registered in the graceful.StopIntake phase, and if
// serving fails, g shuts the service down. Listening happens before Serve
// returns, so an unusable address is reported immediately.
func Serve(g *graceful.Manager, srv *http.Server) error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		return err
	}
	if err := g.Register(graceful.StopIntake, "http "+srv.Addr, srv.Shutdown); err != nil {
		_ = ln.Close()
		return err
	}
	g.Go("http "+srv.Addr, func() error {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	return nil
}
