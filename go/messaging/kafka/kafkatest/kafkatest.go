// Package kafkatest gives tests a Kafka cluster: franz-go's in-process
// kfake by default, or the brokers in KAFKA_TEST_BROKERS
// (comma-separated) to test against real Kafka.
//
//	k := kafkatest.New(t, 3, "orders")                 // 3 partitions
//	p, _ := kafka.NewProducer(ctx, k.Config)
//	c, _ := kafka.NewConsumer(ctx, k.Config, k.Group("billing"), []string{k.Topic("orders")}, h)
//
// Topic and group names get a per-test prefix ("commontest-<test>-<random>-"),
// so tests can share a real broker; the test's topics and groups are
// deleted when it ends, and nothing else on the broker is touched.
package kafkatest

import (
	"context"
	"crypto/rand"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/messaging/kafka"
)

// EnvVar names the variable with real brokers to test against.
const EnvVar = "KAFKA_TEST_BROKERS"

// Env is a Kafka cluster for one test.
type Env struct {
	// Config connects to the cluster.
	Config kafka.Config
	// Prefix starts every topic and group name of the test.
	Prefix string

	mu     sync.Mutex
	groups []string
}

// Topic returns the test's name for topic.
func (e *Env) Topic(topic string) string { return e.Prefix + topic }

// Group returns the test's name for a consumer group, and deletes the
// group when the test ends.
func (e *Env) Group(group string) string {
	name := e.Prefix + group
	e.TrackGroup(name)
	return name
}

// TrackGroup deletes the group name when the test ends, for groups named
// without Group, such as kafka.WithRetryTopics' "<group>.<topic>". On a
// real broker only names starting with Prefix are deleted.
func (e *Env) TrackGroup(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.HasPrefix(name, e.Prefix) && !slices.Contains(e.groups, name) {
		e.groups = append(e.groups, name)
	}
}

// New creates topics with partitions partitions each and returns the
// cluster. With KAFKA_TEST_BROKERS unset it is an in-process kfake cluster.
func New(t testing.TB, partitions int32, topics ...string) *Env {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, t.Name())
	e := &Env{Prefix: "commontest-" + safe + "-" + strings.ToLower(rand.Text()[:6]) + "-"}
	names := make([]string, len(topics))
	for i, topic := range topics {
		names[i] = e.Topic(topic)
	}

	brokers := os.Getenv(EnvVar)
	if brokers == "" {
		c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, names...))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		e.Config = kafka.Config{Brokers: c.ListenAddrs()}
		return e
	}

	e.Config = kafka.Config{Brokers: strings.Split(brokers, ",")}
	cl, err := kgo.NewClient(kgo.SeedBrokers(e.Config.Brokers...))
	if err != nil {
		t.Fatal(err)
	}
	adm := kadm.NewClient(cl)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := adm.CreateTopics(ctx, partitions, -1, nil, names...)
	if err != nil {
		t.Fatalf("create topics: %v", err)
	}
	for _, r := range created.Sorted() {
		if r.Err != nil {
			t.Fatalf("create topic %s: %v", r.Topic, r.Err)
		}
	}
	t.Cleanup(func() { // runs after the test's consumers are closed
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e.mu.Lock()
		groups := slices.Clone(e.groups)
		e.mu.Unlock()
		if len(groups) > 0 {
			if _, err := adm.DeleteGroups(ctx, groups...); err != nil {
				t.Logf("delete groups: %v", err)
			}
		}
		if _, err := adm.DeleteTopics(ctx, names...); err != nil {
			t.Logf("delete topics: %v", err)
		}
		cl.Close()
	})
	// Wait until every partition has a leader, so the first produce does
	// not race topic creation.
	for {
		md, err := adm.Metadata(ctx, names...)
		ready := err == nil
		for _, name := range names {
			td, ok := md.Topics[name]
			if !ok || td.Err != nil || len(td.Partitions) != int(partitions) {
				ready = false
				break
			}
			for _, p := range td.Partitions {
				if p.Leader < 0 || p.Err != nil {
					ready = false
				}
			}
		}
		if ready {
			return e
		}
		if ctx.Err() != nil {
			t.Fatalf("topics %v not ready: %v", names, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Consume reads topic from the beginning with a group of its own until it
// has n records, and returns them. It fails the test after 30s.
func Consume(t testing.TB, e *Env, topic string, n int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(e.Config.Brokers...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("%d of %d records on %s after 30s", len(out), n, topic)
		}
		fs.EachRecord(func(r *kgo.Record) { out = append(out, r) })
	}
	return out
}
