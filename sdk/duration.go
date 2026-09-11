package lore

import (
	"strconv"
	"strings"
	"time"
)

// ParseDuration accepts everything time.ParseDuration does, plus a whole-day
// "30d" that it rejects.
func ParseDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(raw)
}
