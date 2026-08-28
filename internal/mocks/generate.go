// Package mocks holds the go:generate directives for every gomock double the
// test suites consume; `make gen.mock` regenerates them all.
package mocks

//go:generate go tool mockgen -destination repositories/indexstore.go -package mock_repositories lore/internal/repositories IndexStore
//go:generate go tool mockgen -destination entities/connector.go -package mock_entities lore/internal/entities Connector
//go:generate go tool mockgen -destination embedder/embedder.go -package mock_embedder lore/internal/connectors/embedder Embedder
//go:generate go tool mockgen -destination services/chunker.go -package mock_services lore/internal/services Chunker
