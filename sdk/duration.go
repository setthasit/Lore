package lore

import (
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses the duration form Lore accepts everywhere: everything
// time.ParseDuration accepts, plus a whole-day "30d" that it rejects. Event
// windows are naturally spoken in days, and a host and a plugin must agree on
// the spelling, so the parser lives in the contract rather than in either side.
func ParseDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(raw)
}
