# syntax=docker/dockerfile:1
# Controller image: the production build by default, the development image with
# --build-arg GO_TAGS=scarpenter_dev. Built for the architecture of the build
# host by default; `make image-buildx` builds multi-arch images (BUILDPLATFORM
# runs the Go toolchain natively, the binary is cross-compiled for TARGETARCH).
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm@sha256:5de7c99eb78ca5e515a4c340235d0955ec5964a5071c492c15503d7c267da473 AS builder
ARG TARGETOS
ARG TARGETARCH
# Set by the Makefile (git describe) and the release workflow (the git tag);
# nothing in the repository carries a version number.
ARG VERSION=dev
ARG COMMIT=
# Build tags. Empty is the production build (the drivers of the supported
# vendors only); the development image of the test lanes (the fake and qemu
# drivers) is built with GO_TAGS=scarpenter_dev.
ARG GO_TAGS=

WORKDIR /workspace
COPY go.mod go.sum ./
# The modules stay in this layer (no cache mount), so the layer cache, also the
# GitHub Actions one, skips the download while go.mod is unchanged. The build
# cache mount only speeds up local rebuilds (the gha cache does not keep mounts).
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -tags "${GO_TAGS}" \
      -ldflags="-s -w -X github.com/SpareCores/scarpenter/internal/version.Version=${VERSION} -X github.com/SpareCores/scarpenter/internal/version.Commit=${COMMIT}" \
      -o controller ./cmd/controller

# Pinned by digest (the nonroot tag moves); bump together with the builder image.
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
WORKDIR /
COPY --from=builder /workspace/controller .
USER 65532:65532
ENTRYPOINT ["/controller"]
