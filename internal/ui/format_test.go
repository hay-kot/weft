package ui_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hay-kot/weft/internal/ui"
)

func TestFormatDuration(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:            "0:00",
		0:                       "0:00",
		1500 * time.Millisecond: "0:02",
		61 * time.Second:        "1:01",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
	}
	for d, want := range tests {
		assert.Equal(t, want, ui.FormatDuration(d), d.String())
	}
}
