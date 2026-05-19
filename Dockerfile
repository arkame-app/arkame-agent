# syntax=docker/dockerfile:1.10
# =============================================================================
# Arkame Agent — Dockerfile multi-stage
# =============================================================================

# ---------- stage 1: build ----------
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache ca-certificates git make

WORKDIR /src

# Cache deps primeiro
COPY go.mod go.sum* ./
RUN go mod download

# Source
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X github.com/arkame-app/agent/pkg/version.Version=${VERSION} \
      -X github.com/arkame-app/agent/pkg/version.Commit=${COMMIT} \
      -X github.com/arkame-app/agent/pkg/version.BuildDate=${BUILD_DATE}" \
    -o /out/arkame-agent \
    ./cmd/arkame-agent

# ---------- stage 2: runtime ----------
# Usamos distroless para footprint pequeno + CA certs embutidos
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/arkame-agent /usr/local/bin/arkame-agent

# Monte a raiz do servidor em /host (read-only)
VOLUME ["/host"]

# /etc/arkame lê o env-file com credenciais de storage
VOLUME ["/etc/arkame"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/arkame-agent"]
CMD ["run", "--host-root", "/host", "--config", "/etc/arkame/agent.env"]
