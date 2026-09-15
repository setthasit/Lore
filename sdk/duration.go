package lore

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const maxDurationDays = math.MaxInt64 / int64(24*time.Hour)

// ParseDuration accepts everything time.ParseDuration does, plus a whole-day
// "30d" that it rejects, for day counts within ±106751.
func ParseDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.ParseInt(days, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
			if -maxDurationDays <= n && n <= maxDurationDays {
				return time.Duration(n) * 24 * time.Hour, nil
			}
			return 0, fmt.Errorf("out of range: at most %dd, and no fewer than -%dd", maxDurationDays, maxDurationDays)
		}
	}
	return time.ParseDuration(raw)
}
