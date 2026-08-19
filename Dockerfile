# syntax=docker/dockerfile:1

ARG PLAYWRIGHT_VERSION
FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH
ARG CINEKO_VERSION=0.0.0-dev
ARG CINEKO_BROWSER_REVISION=1228
ARG PLAYWRIGHT_VERSION

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -mod=vendor -trimpath \
      -ldflags "-s -w -X main.version=${CINEKO_VERSION} -X main.browserRevision=${CINEKO_BROWSER_REVISION}" \
      -o /out/cineko-probe ./cmd/cineko-probe

# playwright-go installs its matching driver separately from the browser.
# The final official image already owns the target-architecture browser and OS libraries.
RUN PLAYWRIGHT_GO_VERSION="$(bash scripts/playwright-version.sh go)" && \
    PLAYWRIGHT_DRIVER_VERSION="$(bash scripts/playwright-version.sh driver)" && \
    test "$PLAYWRIGHT_DRIVER_VERSION" = "$PLAYWRIGHT_VERSION" && \
    go run "github.com/mxschmitt/playwright-go/cmd/playwright@${PLAYWRIGHT_GO_VERSION}" install --dry-run chromium && \
    mkdir -p /out/playwright && \
    cp -a "/root/.cache/ms-playwright-go/${PLAYWRIGHT_DRIVER_VERSION}/package" /out/playwright/package

FROM mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble

RUN groupadd --system --gid 10001 cineko && \
    useradd --system --uid 10001 --gid cineko --home-dir /var/lib/cineko-probe --shell /usr/sbin/nologin cineko && \
    install -d -o cineko -g cineko -m 0700 /var/lib/cineko-probe /var/lib/cineko-probe/artifacts /tmp/cineko && \
    install -d -o root -g root -m 0755 /opt/cineko/playwright

COPY --from=builder --chown=root:root /out/cineko-probe /usr/local/bin/cineko-probe
COPY --from=builder --chown=root:root /out/playwright/package /opt/cineko/playwright/package
RUN ln -s /usr/bin/node /opt/cineko/playwright/node

ENV CINEKO_PROBE_MODE=container \
    CINEKO_PROBE_DATA_DIR=/var/lib/cineko-probe \
    CINEKO_PLAYWRIGHT_DRIVER_PATH=/opt/cineko/playwright \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    TMPDIR=/tmp/cineko

VOLUME ["/var/lib/cineko-probe"]
USER 10001:10001
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=6 \
  CMD ["/usr/local/bin/cineko-probe", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/cineko-probe"]
