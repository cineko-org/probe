.PHONY: build check contract-check contract-release-check coverage install-playwright lint live-smoke security stealth-check test workflow-check

GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION ?= v1.7.10
GO ?= GOWORK=off go
GO_FILES := $(shell find cmd internal probe -name '*.go' -type f)
CINEKO_LOCAL_DATA_DIR ?= $(if $(strip $(CINEKO_DATA_DIR)),$(CINEKO_DATA_DIR),$(HOME)/cineko)
PLAYWRIGHT_DRIVER_VERSION ?= $(shell bash scripts/playwright-version.sh driver)
PLAYWRIGHT_ROOT ?= $(CINEKO_LOCAL_DATA_DIR)/runtime/playwright
PLAYWRIGHT_DRIVER_DIR ?= $(PLAYWRIGHT_ROOT)/driver/$(PLAYWRIGHT_DRIVER_VERSION)
PLAYWRIGHT_BROWSERS_DIR ?= $(PLAYWRIGHT_ROOT)/browsers
CINEKO_TMP_DIR ?= $(CINEKO_LOCAL_DATA_DIR)/tmp

install-playwright:
	mkdir -p "$(PLAYWRIGHT_DRIVER_DIR)" "$(PLAYWRIGHT_BROWSERS_DIR)" "$(CINEKO_TMP_DIR)"
	PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_BROWSERS_PATH="$(PLAYWRIGHT_BROWSERS_DIR)" \
		TMPDIR="$(CINEKO_TMP_DIR)" \
		$(GO) run github.com/mxschmitt/playwright-go/cmd/playwright@$$(bash scripts/playwright-version.sh go) install chromium

build:
	$(GO) test -mod=vendor ./probe -run '^$$'

lint:
	@test -z "$$(gofmt -l $(GO_FILES))" || (gofmt -l $(GO_FILES) && exit 1)
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

security:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

coverage: install-playwright
	CINEKO_PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_BROWSERS_PATH="$(PLAYWRIGHT_BROWSERS_DIR)" \
		TMPDIR="$(CINEKO_TMP_DIR)" \
		bash scripts/unit-coverage.sh

test: install-playwright
	CINEKO_PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_BROWSERS_PATH="$(PLAYWRIGHT_BROWSERS_DIR)" \
		TMPDIR="$(CINEKO_TMP_DIR)" \
		$(GO) test -mod=vendor -race ./...

live-smoke: install-playwright
	@test -n "$(CINEKO_SOXY_URL)"
	@test -n "$(CINEKO_SOXY_API_TOKEN_FILE)"
	CINEKO_LIVE_CATALOG=1 \
		CINEKO_LIVE_SCHEDULE=1 \
		CINEKO_LIVE_SEAT_MAP=1 \
		CINEKO_PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_DRIVER_PATH="$(PLAYWRIGHT_DRIVER_DIR)" \
		PLAYWRIGHT_BROWSERS_PATH="$(PLAYWRIGHT_BROWSERS_DIR)" \
		TMPDIR="$(CINEKO_TMP_DIR)" \
		$(GO) test -mod=vendor -race ./internal/provider/cgv \
		-run 'TestLive(Catalog|Schedule|SeatMap)Capture' -count=1 -v

contract-check:
	@! grep -Eq '^replace github.com/cineko-org/contracts/v3' go.mod
	@test "$$(grep -Ec '^[[:space:]]*github.com/cineko-org/contracts/v3[[:space:]]+v3\.7\.0([[:space:]]*//[[:space:]]*indirect)?[[:space:]]*$$' go.mod)" -eq 1
	@test "$$(grep -Ec '^[[:space:]]*github.com/cineko-org/contracts(/v[0-9]+)?[[:space:]]' go.mod)" -eq 1
	@test "$$(grep -Ec '^# github.com/cineko-org/contracts/v3 v3\.7\.0$$' vendor/modules.txt)" -eq 1
	@test "$$(grep -Ec '^# github.com/cineko-org/contracts(/v[0-9]+)? v' vendor/modules.txt)" -eq 1
	@! grep -R -En --include='*.go' 'github.com/cineko-org/contracts(/v[0-9]+)?/' cmd internal probe | grep -Ev 'github.com/cineko-org/contracts/v3/'

contract-release-check: contract-check

workflow-check:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) .github/workflows/*.yml

stealth-check:
	bash scripts/check-stealth-provenance.sh

check: lint security coverage test contract-release-check workflow-check stealth-check
