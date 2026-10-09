// Package sample is envdoc test input.
package sample

import (
	"time"

	"github.com/Arif9878/common/go/config"
)

// Config configures the sample. It has a second sentence.
type Config struct {
	// Brokers are the seed brokers.
	Brokers []string `env:"BROKERS,required" envSeparator:","`
	// Timeout bounds each call | with a pipe.
	Timeout  time.Duration `env:"TIMEOUT" envDefault:"10s"`
	Password config.Secret `env:"PASSWORD"` // Password is a secret.
	Ignored  string        `json:"ignored"`
	internal string        `env:"INTERNAL"`
	Log      Nested        `envPrefix:"LOG_"`
}

// Nested has no env fields of its own besides one.
type Nested struct {
	Level string `env:"LEVEL" envDefault:"info"`
}

// OnlyPrefixes has no variables of its own and is not listed.
type OnlyPrefixes struct {
	Log Nested `envPrefix:"LOG_"`
}
