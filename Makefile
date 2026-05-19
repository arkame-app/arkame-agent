.PHONY: build build-all test lint clean docker run help

BINARY     := arkame-agent
PKG        := github.com/arkame-app/agent
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/pkg/version.Version=$(VERSION) \
	-X $(PKG)/pkg/version.Commit=$(COMMIT) \
	-X $(PKG)/pkg/version.BuildDate=$(BUILD_DATE)

help: ## Lista os targets disponíveis
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Compila o binário para a plataforma atual
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/arkame-agent

build-all: ## Cross-compile para todas as plataformas suportadas
	mkdir -p bin
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)_linux_amd64   ./cmd/arkame-agent
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)_linux_arm64   ./cmd/arkame-agent
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)_darwin_amd64  ./cmd/arkame-agent
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)_darwin_arm64  ./cmd/arkame-agent
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)_windows_amd64.exe ./cmd/arkame-agent

test: ## Roda testes
	go test -race -coverprofile=coverage.out ./...

lint: ## Lint + vet
	go vet ./...
	gofmt -l -d . | tee /dev/stderr | grep -q . && exit 1 || true

clean: ## Remove binários e coverage
	rm -rf bin coverage.out

docker: ## Build da imagem Docker
	docker build -t arkame/agent:$(VERSION) -t arkame/agent:latest .

run: build ## Compila e roda local (requer .env pré-configurado)
	./bin/$(BINARY) run --config /etc/arkame/agent.env

.DEFAULT_GOAL := help
