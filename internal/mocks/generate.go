package mocks

//go:generate go tool mockgen -destination repositories/indexstore.go -package mock_repositories github.com/setthasit/Lore/internal/repositories IndexStore
//go:generate go tool mockgen -destination entities/connector.go -package mock_entities github.com/setthasit/Lore/internal/entities Connector
//go:generate go tool mockgen -destination embedder/embedder.go -package mock_embedder github.com/setthasit/Lore/internal/connectors/embedder Embedder
//go:generate go tool mockgen -destination llm/llm.go -package mock_llm github.com/setthasit/Lore/internal/connectors/llm LLM
//go:generate go tool mockgen -destination gitrepo/gitrepo.go -package mock_gitrepo github.com/setthasit/Lore/internal/connectors/gitrepo GitRepo
//go:generate go tool mockgen -destination services/chunker.go -package mock_services github.com/setthasit/Lore/internal/services Chunker
//go:generate go tool mockgen -destination services/linkresolver.go -package mock_services github.com/setthasit/Lore/internal/services LinkResolver
//go:generate go tool mockgen -destination services/query.go -package mock_services github.com/setthasit/Lore/internal/services QueryService
//go:generate go tool mockgen -destination services/why.go -package mock_services github.com/setthasit/Lore/internal/services WhyService
//go:generate go tool mockgen -destination services/trace.go -package mock_services github.com/setthasit/Lore/internal/services TraceService
//go:generate go tool mockgen -destination services/impact.go -package mock_services github.com/setthasit/Lore/internal/services ImpactService
//go:generate go tool mockgen -destination services/history.go -package mock_services github.com/setthasit/Lore/internal/services HistoryService
//go:generate go tool mockgen -destination services/sync.go -package mock_services github.com/setthasit/Lore/internal/services SyncOrchestrator
//go:generate go tool mockgen -destination services/status.go -package mock_services github.com/setthasit/Lore/internal/services StatusService
//go:generate go tool mockgen -destination services/synthesis.go -package mock_services github.com/setthasit/Lore/internal/services SynthesisService
