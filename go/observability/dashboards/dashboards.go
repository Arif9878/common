// Package dashboards holds a Grafana dashboard and Prometheus alerting
// rules for the metrics the common library's packages emit, so every
// service gets the same views and alerts without writing queries:
//
//	grafana/service-overview.json   HTTP, gRPC, outgoing calls, Kafka, outbox,
//	                                PostgreSQL pool, resilience, Go runtime
//	prometheus/alerts.yaml          error rates, latency, stopped partitions,
//	                                consumer lag, dead-lettering, outbox relay,
//	                                pool exhaustion, open breakers, expiring
//	                                Vault tokens and credentials
//
// Import the dashboard into Grafana (or provision it from the file); it
// selects services by the job label, which Prometheus sets when scraping
// and Mimir or Grafana Cloud derive from service.name when metrics are
// pushed over OTLP. Load the rules into Prometheus, Mimir or the Prometheus
// Operator (as a PrometheusRule's spec.groups).
//
// The files are also embedded in [FS], for tools that provision them. A
// test checks that every metric they query is one the library emits, so a
// renamed metric fails the build instead of leaving an empty panel.
package dashboards

import "embed"

// FS holds grafana/service-overview.json and prometheus/alerts.yaml.
//
//go:embed grafana/*.json prometheus/*.yaml
var FS embed.FS
