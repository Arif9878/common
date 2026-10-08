package config_test

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/tracing"
)

const secretValue = "hunter2-s3cr3t"

type DatabaseConfig struct {
	Host     string        `env:"HOST,required"`
	Port     int           `env:"PORT" envDefault:"5432" validate:"min=1,max=65535"`
	User     string        `env:"USER" envDefault:"app"`
	Password config.Secret `env:"PASSWORD,required"`
	Timeout  time.Duration `env:"TIMEOUT" envDefault:"5s"`
}

type AppConfig struct {
	Name string   `env:"NAME" envDefault:"orders"`
	Mode string   `env:"MODE" envDefault:"api" validate:"oneof=api worker"`
	Tags []string `env:"TAGS" envSeparator:","`
}

type Config struct {
	App      AppConfig       `envPrefix:"APP_"`
	Database DatabaseConfig  `envPrefix:"DB_"`
	Log      logging.Config  `envPrefix:"LOG_"`
	Tracing  tracing.Config  `envPrefix:"TRACING_"`
	Metrics  metrics.Config  `envPrefix:"METRICS_"`
	Replica  *DatabaseConfig `envPrefix:"REPLICA_"`
}

func validEnv() map[string]string {
	return map[string]string{
		"DB_HOST":     "db.internal",
		"DB_PASSWORD": secretValue,
	}
}

func with(base map[string]string, kv ...string) map[string]string {
	out := maps.Clone(base)
	for pair := range slices.Chunk(kv, 2) {
		out[pair[0]] = pair[1]
	}
	return out
}

func TestLoadDefaultsAndNesting(t *testing.T) {
	cfg, err := config.Load[Config](config.WithEnvironment(with(validEnv(),
		"APP_TAGS", "a,b",
		"DB_TIMEOUT", "250ms",
		"LOG_LEVEL", "debug",
		"TRACING_SAMPLE_RATIO", "0.25",
	)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.App.Name != "orders" || cfg.App.Mode != "api" || strings.Join(cfg.App.Tags, "|") != "a|b" {
		t.Errorf("App = %+v", cfg.App)
	}
	if cfg.Database.Host != "db.internal" || cfg.Database.Port != 5432 || cfg.Database.Timeout != 250*time.Millisecond {
		t.Errorf("Database = %+v", cfg.Database)
	}
	if cfg.Database.Password.Reveal() != secretValue {
		t.Error("Password not loaded")
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v", cfg.Log)
	}
	if cfg.Tracing.Exporter != "none" || cfg.Tracing.SampleRatio == nil || *cfg.Tracing.SampleRatio != 0.25 {
		t.Errorf("Tracing = %+v", cfg.Tracing)
	}
	if cfg.Replica != nil {
		t.Errorf("Replica = %+v, want nil (env only allocates pointers tagged init)", cfg.Replica)
	}
}

func TestLoadPrefix(t *testing.T) {
	cfg, err := config.Load[Config](config.WithPrefix("ORDERS_"), config.WithEnvironment(map[string]string{
		"ORDERS_DB_HOST":     "h",
		"ORDERS_DB_PASSWORD": "p",
		"DB_HOST":            "ignored",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Host != "h" {
		t.Errorf("Host = %q", cfg.Database.Host)
	}
}

func TestLoadProcessEnvironment(t *testing.T) {
	t.Setenv("DB_HOST", "from-process")
	t.Setenv("DB_PASSWORD", "p")
	cfg, err := config.Load[Config]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Host != "from-process" {
		t.Errorf("Host = %q", cfg.Database.Host)
	}
}

func TestLoadSecretFromFile(t *testing.T) {
	type C struct {
		Token config.Secret `env:"TOKEN_FILE,file,required"`
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(secretValue), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load[C](config.WithEnvironment(map[string]string{"TOKEN_FILE": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token.Reveal() != secretValue {
		t.Error("token not read from file")
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := config.Load[Config](config.WithEnvironment(map[string]string{
		"DB_PORT": secretValue, // a secret pasted into the wrong variable
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"DB_HOST", "DB_PASSWORD", "Port", "invalid syntax"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, secretValue) {
		t.Fatalf("error leaks value: %s", msg)
	}
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("kind = %v", errors.KindOf(err))
	}

	// Deterministic: same input, same message.
	_, err2 := config.Load[Config](config.WithEnvironment(map[string]string{"DB_PORT": secretValue}))
	if err2.Error() != msg {
		t.Errorf("non-deterministic error:\n%s\n%s", msg, err2)
	}
}

func TestLoadValidation(t *testing.T) {
	_, err := config.Load[Config](config.WithEnvironment(with(validEnv(),
		"DB_PORT", "70000",
		"APP_MODE", "batch",
		"LOG_FORMAT", "xml",
		"TRACING_EXPORTER", "zipkin",
		"METRICS_CARDINALITY_LIMIT", "-1",
	)))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"Database.Port: must satisfy max=65535",
		"App.Mode: must satisfy oneof=api worker",
		"Log: invalid format",
		"Tracing: unknown exporter",
		"Metrics: cardinality limit is negative",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
	for _, leaked := range []string{"70000", "batch", "xml", "zipkin"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("error leaks value %q:\n%s", leaked, msg)
		}
	}
}

type selfChecking struct {
	Min int `env:"MIN" envDefault:"5"`
	Max int `env:"MAX" envDefault:"10"`
}

func (c selfChecking) Validate() error {
	if c.Min > c.Max {
		return stderrors.New("min must not exceed max")
	}
	return nil
}

type rootChecking struct {
	Limits selfChecking `envPrefix:"LIMITS_"`
	Name   string       `env:"NAME"`
}

func (c *rootChecking) Validate() error {
	if c.Name == "" {
		return stderrors.New("name is required in this deployment")
	}
	return nil
}

func TestValidateMethods(t *testing.T) {
	_, err := config.Load[rootChecking](config.WithEnvironment(map[string]string{"LIMITS_MIN": "20"}))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	rootIdx := strings.Index(msg, "name is required")
	nestedIdx := strings.Index(msg, "Limits: min must not exceed max")
	if rootIdx < 0 || nestedIdx < 0 || rootIdx > nestedIdx {
		t.Fatalf("want root then nested error, got:\n%s", msg)
	}

	if _, err := config.Load[rootChecking](config.WithEnvironment(map[string]string{"NAME": "x"})); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateDirect(t *testing.T) {
	valid := Config{App: AppConfig{Mode: "api"}, Database: DatabaseConfig{Port: 5432}}
	if err := config.Validate(valid); err != nil {
		t.Errorf("valid struct rejected: %v", err)
	}

	// Nested pointers are validated when set and skipped when nil.
	withReplica := valid
	withReplica.Replica = &DatabaseConfig{Port: 0}
	err := config.Validate(&withReplica)
	if err == nil || !strings.Contains(err.Error(), "Replica.Port: must satisfy min=1") {
		t.Errorf("Validate = %v, want Replica.Port error", err)
	}
	if err := config.Validate(42); err == nil {
		t.Error("non-struct accepted")
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := config.Secret(secretValue)
	type holder struct {
		S config.Secret
		P *config.Secret
	}
	h := holder{S: s, P: &s}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10s", "%-5v"} {
		outputs = append(outputs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, h))
	}
	outputs = append(outputs, fmt.Sprint(s), fmt.Sprintln(h), s.String(), s.GoString())

	j, _ := json.Marshal(h)
	outputs = append(outputs, string(j))
	txt, _ := s.MarshalText()
	outputs = append(outputs, string(txt))

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "s", s, "h", h)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "s", s, "h", h)
	outputs = append(outputs, buf.String())

	for _, out := range outputs {
		if strings.Contains(out, secretValue) || strings.Contains(out, fmt.Sprintf("%x", secretValue)) {
			t.Errorf("secret leaked: %s", out)
		}
	}
	if s.Reveal() != secretValue {
		t.Error("Reveal changed the value")
	}
}

func TestLogValue(t *testing.T) {
	type C struct {
		Name     string
		Password string // not a Secret: redacted by the logging handler's key rules
		Token    config.Secret
		Timeout  time.Duration
		Started  time.Time
		Nested   *DatabaseConfig
		Missing  *DatabaseConfig
		internal string
	}
	c := C{
		Name:     "orders",
		Password: "plain-" + secretValue,
		Token:    secretValue,
		Timeout:  time.Second,
		Started:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Nested:   &DatabaseConfig{Host: "db", Password: secretValue},
		internal: "hidden",
	}

	var buf bytes.Buffer
	logger, err := logging.New(logging.Config{}, logging.WithWriter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("starting", "config", config.LogValue(&c))

	out := buf.String()
	if strings.Contains(out, secretValue) || strings.Contains(out, "hidden") {
		t.Fatalf("leaked: %s", out)
	}
	var rec struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Config["Name"] != "orders" || rec.Config["Started"] != "2026-01-02T03:04:05Z" {
		t.Errorf("config = %v", rec.Config)
	}
	nested, _ := rec.Config["Nested"].(map[string]any)
	if nested["Host"] != "db" || nested["Port"] != float64(0) {
		t.Errorf("Nested = %v", nested)
	}
	if rec.Config["Missing"] != nil {
		t.Errorf("Missing = %v", rec.Config["Missing"])
	}
}
