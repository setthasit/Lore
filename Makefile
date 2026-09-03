.PHONY: build test lint gen.mock gen.proto

build:
	go build ./...

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
