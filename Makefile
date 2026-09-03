.PHONY: build bin build.matrix test lint gen.mock gen.proto certs.dev

MODULE  := github.com/setthasit/Lore
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

STAMP := -X $(MODULE)/internal/transport/cli.version=$(VERSION) \
	-X $(MODULE)/internal/transport/cli.commit=$(COMMIT) \
	-X $(MODULE)/internal/transport/cli.buildDate=$(DATE)

# One row per binary Lore claims to ship.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

build:
	go build ./...

bin:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(STAMP)" -o bin/lore ./cmd/lore

# Proof of the pure-Go claim: every target builds with cgo switched off. A
# failure here is a driver problem, not something to work around per platform.
build.matrix:
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		echo "==> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(STAMP)" \
			-o dist/lore-$$os-$$arch$$ext ./cmd/lore; \
	done

test:
	go test ./...

lint:
	golangci-lint run

gen.mock:
	go generate ./...

gen.proto:
	protoc --proto_path=api/proto \
		--plugin=protoc-gen-go=$$(go tool -n protoc-gen-go) \
		--go_out=api/proto --go_opt=paths=source_relative \
		--go-grpc_out=api/proto --go-grpc_opt=paths=source_relative \
		lore/v1/lore.proto

certs.dev:
	./scripts/certs-dev.sh
