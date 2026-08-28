.PHONY: build test lint gen.mock

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

gen.mock:
	go generate ./...
