.PHONY: build check container-check contract-check contract-release-check coverage lint platform-check security test workflow-check

GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION ?= v1.7.10
GO_FILES := $(shell find cmd internal probe -name '*.go' -type f)

build:
	mkdir -p bin
	go build -mod=vendor -trimpath -ldflags "-s -w" -o bin/cineko-probe ./cmd/cineko-probe

container-check:
	docker build --build-arg PLAYWRIGHT_VERSION="$$(bash scripts/playwright-version.sh driver)" --tag cineko-probe:local .
	bash scripts/check-container-runtime.sh cineko-probe:local

lint:
	@test -z "$$(gofmt -l $(GO_FILES))" || (gofmt -l $(GO_FILES) && exit 1)
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

security:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

coverage:
	bash scripts/unit-coverage.sh

test:
	go test -mod=vendor -race ./...

contract-check:
	grep -Eq '^# github.com/cineko-org/contracts/v3 v3.2.1( => ../contracts)?$$' vendor/modules.txt

contract-release-check:
	@! grep -Eq '^[[:space:]]*replace([[:space:]]|\()' go.mod
	@grep -Eq '^[[:space:]]*github.com/cineko-org/contracts/v3 v3.2.1$$' go.mod
	@grep -Eq '^# github.com/cineko-org/contracts/v3 v3.2.1$$' vendor/modules.txt
	@grep -Eq '^github.com/cineko-org/contracts/v3 v3.2.1 h1:' go.sum

workflow-check:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) .github/workflows/*.yml

platform-check:
	bash scripts/check-probe-platforms.sh

check: lint security coverage test contract-release-check workflow-check platform-check
