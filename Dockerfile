# syntax=docker/dockerfile:1

# guild multi-stage Docker build.
#
# Builder stage (golang, bookworm): stages the per-platform embedded
# runtime assets (BGE model + vocab from the pinned model release,
# libonnxruntime from the upstream ONNX Runtime release), then compiles
# a pure-Go (CGO_ENABLED=0) binary with -tags=withembed so semantic
# retrieval ships inside the image. Mirrors `make build` + `make assets`
# without requiring the gh CLI: release assets are fetched with curl.
#
# Runtime stage (debian:bookworm-slim): non-root `guild` user with a
# real home. guild resolves all state under $HOME/.guild, so mounting a
# volume at /home/guild/.guild is the only isolation knob needed:
#
#   docker run --rm -v guild-state:/home/guild/.guild guild:latest --version
#
# debian-slim (not distroless/scratch) is deliberate: the binary dlopens
# the bundled libonnxruntime.so at runtime, which needs glibc and
# libstdc++, and a shell keeps the image debuggable. Correctness beats
# size here.

ARG GO_VERSION=1.26.9

# --platform=$BUILDPLATFORM: run the builder natively and cross-compile
# with GOOS/GOARCH instead of emulating the target architecture.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

# Warm the module cache before copying the tree so source-only changes
# do not re-download dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

# ORT_VERSION pins the ONNX Runtime release; must match the Makefile
# (onnxruntime-purego pins ORT API v23, which ships in 1.23.x).
ARG ORT_VERSION=1.23.0
# MODEL_REPO hosts the model-v<version> release read from .model-version.
ARG MODEL_REPO=mathomhaus/guild

# Stage the three embedded assets for the target platform under
# internal/lore/embed/assets/<goos>_<goarch>/ so the withembed build
# tag can go:embed them. The ${VAR:-fallback} forms keep the build
# working under the classic (non-BuildKit) builder, where TARGETOS and
# TARGETARCH are not auto-populated.
RUN set -eu; \
    goos="${TARGETOS:-linux}"; \
    goarch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${goos}/${goarch}" in \
      linux/amd64) ort_platform="linux-x64" ;; \
      linux/arm64) ort_platform="linux-aarch64" ;; \
      *) echo "unsupported target ${goos}/${goarch} (embed assets exist for linux amd64/arm64 only)" >&2; exit 1 ;; \
    esac; \
    model_version="$(tr -d '[:space:]' < .model-version)"; \
    assets_dir="internal/lore/embed/assets/${goos}_${goarch}"; \
    mkdir -p "${assets_dir}"; \
    echo "staging model-v${model_version} assets into ${assets_dir}"; \
    curl -fsSL -o "${assets_dir}/model.onnx" \
      "https://github.com/${MODEL_REPO}/releases/download/model-v${model_version}/model.onnx"; \
    curl -fsSL -o "${assets_dir}/vocab.txt" \
      "https://github.com/${MODEL_REPO}/releases/download/model-v${model_version}/vocab.txt"; \
    curl -fsSL -o /tmp/onnxruntime.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-${ort_platform}-${ORT_VERSION}.tgz"; \
    mkdir -p /tmp/onnxruntime; \
    tar -xzf /tmp/onnxruntime.tgz -C /tmp/onnxruntime; \
    lib="$(find /tmp/onnxruntime -type f -name "libonnxruntime.so.${ORT_VERSION}" | head -n1)"; \
    test -n "${lib}" || { echo "libonnxruntime.so.${ORT_VERSION} not found in ORT release tarball" >&2; exit 1; }; \
    cp "${lib}" "${assets_dir}/libonnxruntime.so"; \
    rm -rf /tmp/onnxruntime /tmp/onnxruntime.tgz

# Version stamp build args; the Makefile docker-build target passes the
# same values `make build` would.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# -mod=mod: ignore any vendor/ directory that leaked into the context
# from a dev checkout; module mode is the source of truth here.
RUN set -eu; \
    goos="${TARGETOS:-linux}"; \
    goarch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -mod=mod -tags=withembed \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/guild ./cmd/guild

# ---------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------

FROM debian:bookworm-slim

# libstdc++6: required by the bundled libonnxruntime.so that the binary
# extracts to ~/.cache/guild/runtime/ and dlopens on first use.
# ca-certificates: TLS roots for the optional release update check.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
 && rm -rf /var/lib/apt/lists/*

# Non-root user with a real home: guild resolves every state path under
# os.UserHomeDir(). Pre-create ~/.guild (0700, guild-owned) so a named
# volume mounted there inherits the right ownership on first use.
RUN useradd --create-home --uid 65532 guild \
 && install -d -m 0700 -o guild -g guild /home/guild/.guild

COPY --from=build /out/guild /usr/local/bin/guild

USER guild
ENV HOME=/home/guild
WORKDIR /home/guild

ENTRYPOINT ["guild"]
CMD ["--help"]
