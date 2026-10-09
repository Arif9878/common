GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0

.PHONY: check tidy vet lint test vuln test-kafka release

check: tidy vet lint test vuln

tidy:
	go mod tidy -diff

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

test:
	go test -race -shuffle=on -count=1 ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Kafka tests against real brokers instead of the in-process fake, e.g.
#   make test-kafka KAFKA_TEST_BROKERS=192.168.11.196:9092
# Each test creates and deletes its own commontest-* topics and groups.
test-kafka:
	@test -n "$(KAFKA_TEST_BROKERS)" || { echo "set KAFKA_TEST_BROKERS=host:port[,host:port]"; exit 1; }
	KAFKA_TEST_BROKERS=$(KAFKA_TEST_BROKERS) go test -race -count=1 -timeout 10m ./messaging/kafka/

# Release every module of the repository together, e.g.
#   make release VERSION=v0.5.0              # dry run: prints the plan
#   make release VERSION=v0.5.0 PUSH=1       # commit, tag, push, GitHub releases
# See RELEASING.md.
release:
	@test -n "$(VERSION)" || { echo "set VERSION=vX.Y.Z"; exit 1; }
	go run ./internal/release -version $(VERSION) $(if $(PUSH),-push -github-release) $(if $(REMOTE),-remote $(REMOTE))
