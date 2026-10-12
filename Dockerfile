# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Mosaicast Authors
#
# Build: one static binary with the default theme and runtime embedded (stdlib only).
# Runtime: distroless, no shell, non-root, read-only root filesystem friendly.
#
# The build stage runs on the builder's own platform and cross-compiles (Go needs no emulation for that),
# so a multi-platform release (linux/amd64, linux/arm64) tests and checks natively and only the final
# binary differs.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod VERSION ./
COPY *.go ./
COPY internal ./internal
COPY web ./web
COPY examples ./examples
RUN CGO_ENABLED=0 go test ./... \
 && CGO_ENABLED=0 go run . check-theme examples/minimal \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /mosaicast-edge . \
 && mkdir /cache

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /mosaicast-edge /mosaicast-edge
# the policy and site copy survive restarts here; mount a volume (the root filesystem stays read-only)
COPY --from=build --chown=65532:65532 /cache /cache
ENV EDGE_CACHE_DIR=/cache
# the garbage collector's target under compose's 128 MB limit; raise both together
ENV GOMEMLIMIT=96MiB
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/mosaicast-edge", "healthcheck"]
ENTRYPOINT ["/mosaicast-edge"]
