GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0

# Every Go module under this directory: the core module (.), fx, and the
# integration modules (messaging/kafka, datastore/postgres, …). Targets run
# in each; MODULES=messaging/kafka narrows them to one.
MODULES ?= $(shell find . -name go.mod -not -path '*/vendor/*' -not -path '*/testdata/*' -exec dirname {} \; | sort)

# each runs $(1) in every module, stopping at the first failure.
define each
	@set -e; for m in $(MODULES); do echo "== $$m"; (cd $$m && $(1)); done
endef

.PHONY: check tidy vet lint test vuln test-kafka release

check: tidy vet lint test vuln

tidy:
	$(call each,go mod tidy -diff)

vet:
	$(call each,go vet ./...)

lint:
	$(call each,go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --config $(CURDIR)/.golangci.yml ./...)

test:
	$(call each,go test -race -shuffle=on -count=1 ./...)

vuln:
	$(call each,go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...)

# Kafka tests against real brokers instead of the in-process fake, e.g.
#   make test-kafka KAFKA_TEST_BROKERS=192.168.11.196:9092 SCHEMA_REGISTRY_TEST_URL=http://192.168.11.196:8081
# Each test creates and deletes its own commontest-* topics, groups and
# schema subjects. SCHEMA_REGISTRY_TEST_URL is optional.
test-kafka:
	@test -n "$(KAFKA_TEST_BROKERS)" || { echo "set KAFKA_TEST_BROKERS=host:port[,host:port]"; exit 1; }
	cd messaging/kafka && KAFKA_TEST_BROKERS=$(KAFKA_TEST_BROKERS) SCHEMA_REGISTRY_TEST_URL=$(SCHEMA_REGISTRY_TEST_URL) \
		go test -race -count=1 -timeout 10m ./...

# Release every module of the repository together, e.g.
#   make release VERSION=v0.5.0              # dry run: prints the plan
#   make release VERSION=v0.5.0 PUSH=1       # commit, tag, push, GitHub releases
# See RELEASING.md.
release:
	@test -n "$(VERSION)" || { echo "set VERSION=vX.Y.Z"; exit 1; }
	go run ./internal/release -version $(VERSION) $(if $(PUSH),-push -github-release) $(if $(REMOTE),-remote $(REMOTE))
