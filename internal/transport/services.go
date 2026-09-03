package transport

import "github.com/setthasit/Lore/internal/services"

type Services struct {
	Query   services.QueryService
	Why     services.WhyService
	Trace   services.TraceService
	Impact  services.ImpactService
	History services.HistoryService
	Sync    services.SyncOrchestrator
	Status  services.StatusService
}
