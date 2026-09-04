package mocks

//go:generate go tool mockgen -destination repositories/indexstore.go -package mock_repositories github.com/setthasit/Lore/internal/repositories IndexStore
//go:generate go tool mockgen -destination lore/connector.go -package mock_lore github.com/setthasit/Lore/sdk Connector
//go:generate go tool mockgen -destination lore/embedder.go -package mock_lore github.com/setthasit/Lore/sdk Embedder
//go:generate go tool mockgen -destination lore/completer.go -package mock_lore github.com/setthasit/Lore/sdk Completer
//go:generate go tool mockgen -destination lore/coderepo.go -package mock_lore github.com/setthasit/Lore/sdk CodeRepo
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
