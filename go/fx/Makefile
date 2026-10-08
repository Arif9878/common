# Same checks as the core module; the lint configuration is shared.
GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0

.PHONY: check tidy vet lint test vuln

check: tidy vet lint test vuln

tidy:
	go mod tidy -diff

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --config ../.golangci.yml ./...

test:
	go test -race -shuffle=on -count=1 ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
