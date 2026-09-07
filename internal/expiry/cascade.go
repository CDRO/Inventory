package expiry

import "time"

// DaysUntil returns the number of days until t.
func DaysUntil(t time.Time) int {
	return int(time.Until(t).Hours() / 24)
}
