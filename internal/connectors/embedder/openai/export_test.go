package openai

import "lore/internal/connectors/httpretry"

const (
	maxAttempts = httpretry.MaxAttempts
	baseBackoff = httpretry.BaseBackoff
)
