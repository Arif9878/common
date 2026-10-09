package commonfx_test

import (
	"context"
	"embed"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/concurrency/schedule"
	"github.com/Arif9878/common/go/datastore/postgres"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/otlp"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// Config is the service's configuration, loaded from the environment.
type Config struct {
	OTLP     otlp.Config          `envPrefix:"OTLP_"`
	Log      logging.Config       `envPrefix:"LOG_"`
	Logs     logs.Config          `envPrefix:"LOGS_"`
	Tracing  tracing.Config       `envPrefix:"TRACING_"`
	Metrics  metrics.Config       `envPrefix:"METRICS_"`
	HTTP     httpserver.Config    `envPrefix:"HTTP_"`
	Admin    commonfx.AdminConfig `envPrefix:"ADMIN_"`
	Postgres postgres.Config      `envPrefix:"POSTGRES_"`
}

var migrations embed.FS // //go:embed migrations/*.sql in a real service

// A service with telemetry, the admin server, an Echo API, PostgreSQL with
// migrations and a scheduled job. Shutdown runs in the standard phases.
func Example() {
	app := fx.New(
		commonfx.Config[Config](),
		commonfx.ConfigFields[Config](),
		commonfx.Observability(),
		commonfx.Lifecycle(),
		commonfx.AdminServer(), // /live, /ready, /startup, /metrics on :9090
		commonfx.EchoServer(),  // *echo.Echo with the standard middleware and errors
		commonfx.Postgres(),
		commonfx.PostgresMigrations(migrations),
		commonfx.PostgresLocker(), // one replica runs each scheduled job
		commonfx.Scheduler(),
		fx.Invoke(func(e *echo.Echo, db *postgres.DB) {
			e.GET("/orders/:id", func(c echo.Context) error {
				var status string
				err := db.QueryRow(c.Request().Context(), "SELECT status FROM orders WHERE id = $1", c.Param("id")).Scan(&status)
				if err != nil {
					return postgres.Classify(err) // pgx.ErrNoRows becomes a 404
				}
				return c.JSON(http.StatusOK, map[string]string{"status": status})
			})
		}),
		fx.Invoke(func(s *schedule.Scheduler, db *postgres.DB) error {
			return s.Every("expire-carts", 5*time.Minute, func(ctx context.Context) error {
				_, err := db.Exec(ctx, "DELETE FROM carts WHERE expires_at < now()")
				return err
			})
		}),
		commonfx.Ready(), // always last
	)
	_ = app // app.Run() blocks until SIGTERM, then shuts down
}
