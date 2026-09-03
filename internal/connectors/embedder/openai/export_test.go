package openai

import "github.com/setthasit/Lore/internal/connectors/httpretry"

const (
	maxAttempts = httpretry.MaxAttempts
	baseBackoff = httpretry.BaseBackoff
)
