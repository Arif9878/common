package config_test

import (
	"fmt"
	"time"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/observability/logging"
)

func ExampleLoad() {
	type Config struct {
		HTTPAddr    string         `env:"HTTP_ADDR" envDefault:":8080"`
		DatabaseURL config.Secret  `env:"DATABASE_URL,required"`
		Timeout     time.Duration  `env:"TIMEOUT" envDefault:"5s" validate:"min=1ms"`
		Log         logging.Config `envPrefix:"LOG_"`
	}

	// In production, omit WithEnvironment to read the process environment.
	cfg, err := config.Load[Config](
		config.WithPrefix("ORDERS_"),
		config.WithEnvironment(map[string]string{
			"ORDERS_DATABASE_URL": "postgres://db.internal/orders",
			"ORDERS_LOG_LEVEL":    "debug",
		}),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(cfg.HTTPAddr, cfg.Timeout, cfg.Log.Level)
	fmt.Println(cfg.DatabaseURL)
	// Output:
	// :8080 5s debug
	// [REDACTED]
}

func ExampleLoad_errors() {
	type Config struct {
		DatabaseURL config.Secret `env:"DATABASE_URL,required"`
		Workers     int           `env:"WORKERS" envDefault:"4" validate:"min=1"`
	}
	_, err := config.Load[Config](config.WithEnvironment(map[string]string{"WORKERS": "0"}))
	fmt.Println(err)
	// Output:
	// config: required environment variable "DATABASE_URL" is not set
}
