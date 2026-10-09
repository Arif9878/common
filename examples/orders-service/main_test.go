package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/twmb/franz-go/pkg/kfake"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Arif9878/common/go/config"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/testkit/pgtest"

	"github.com/Arif9878/common/examples/orders-service/internal/orders"
)

// TestOrderFlow runs the whole service: POST /orders writes the order and
// its event in one transaction, the outbox relay publishes the event to
// Kafka, the consumer records the notification once, and GET /orders/:id
// shows it. PostgreSQL comes from POSTGRES_TEST_URL; Kafka and Redis are
// in-process.
func TestOrderFlow(t *testing.T) {
	pg := pgtest.Config(t)
	db := pgtest.DB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP TABLE IF EXISTS notifications, orders, kafka_outbox, goose_db_version`)
	})
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, orders.Topic))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	httpAddr, adminAddr := testkit.FreeAddr(t), testkit.FreeAddr(t)

	env := map[string]string{
		"LOG_LEVEL":         "warn",
		"HTTP_ADDR":         httpAddr,
		"ADMIN_ADDR":        adminAddr,
		"POSTGRES_HOST":     pg.Host,
		"POSTGRES_PORT":     strconv.Itoa(pg.Port),
		"POSTGRES_DATABASE": pg.Database,
		"POSTGRES_USER":     pg.User,
		"POSTGRES_PASSWORD": pg.Password.Reveal(),
		"POSTGRES_SSLMODE":  "disable",
		"REDIS_ADDRS":       miniredis.RunT(t).Addr(),
		"KAFKA_BROKERS":     strings.Join(cluster.ListenAddrs(), ","),
	}
	app := fxtest.New(t, append([]fx.Option{commonfx.Config[Config](config.WithEnvironment(env))},
		append(options(), commonfx.Ready())...)...)
	app.RequireStart()
	defer app.RequireStop()

	api := "http://" + httpAddr
	code, body := call(t, http.MethodPost, api+"/orders", `{"customer_id":"c-1","amount":1500}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /orders = %d %s", code, body)
	}
	var created orders.Order
	_ = json.Unmarshal([]byte(body), &created)

	testkit.Eventually(t, 20*time.Second, "the order notification", func() bool {
		_, body := call(t, http.MethodGet, api+"/orders/"+strconv.FormatInt(created.ID, 10), "")
		var o orders.Order
		return json.Unmarshal([]byte(body), &o) == nil && o.Notified
	})

	// Errors are problem+json responses with the status of their kind.
	if code, body := call(t, http.MethodPost, api+"/orders", `{"customer_id":"","amount":0}`); code != http.StatusBadRequest ||
		!strings.Contains(body, `"field":"customer_id"`) || !strings.Contains(body, `"field":"amount"`) {
		t.Errorf("invalid order = %d %s", code, body)
	}
	if code, _ := call(t, http.MethodGet, api+"/orders/999999", ""); code != http.StatusNotFound {
		t.Errorf("unknown order = %d, want 404", code)
	}
	if code, _ := call(t, http.MethodGet, "http://"+adminAddr+"/ready", ""); code != http.StatusOK {
		t.Errorf("/ready = %d", code)
	}
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, url, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
