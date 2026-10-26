// Package ui holds formatting shared by weft's renderers.
package ui

import (
	"fmt"
	"time"
)

// FormatDuration renders d as m:ss, or h:mm:ss past an hour. Negative
// durations render as 0:00.
func FormatDuration(d time.Duration) string {
	d = max(0, d.Round(time.Second))
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
