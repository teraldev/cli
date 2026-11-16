package ui

import (
	"fmt"
	"math"
	"time"
)

// Ago renders a Unix-millisecond timestamp relative to now, e.g. "3m ago".
func Ago(ms int64) string {
	if ms <= 0 {
		return "never"
	}
	return Relative(time.UnixMilli(ms), time.Now())
}

// Relative renders t relative to now.
func Relative(t, now time.Time) string {
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		s = fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// Bytes renders a size, e.g. "1.4 MB".
func Bytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	exp := int(math.Log(float64(n)) / math.Log(unit))
	if exp > 4 {
		exp = 4
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/math.Pow(unit, float64(exp)), "kMGT"[exp-1])
}

// Duration renders an elapsed time, e.g. "1m 12s".
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ShortSHA shortens a commit SHA.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
