package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScrollerWindow(t *testing.T) {
	tests := []struct {
		name               string
		s                  scroller
		first, end, height int
		start, stop        int
	}{
		{"following shows the tail", scroller{top: 0, follow: true}, 0, 100, 10, 90, 100},
		{"following short log shows all", scroller{top: 0, follow: true}, 0, 4, 10, 0, 4},
		{"held position stays", scroller{top: 20, follow: false}, 0, 100, 10, 20, 30},
		{"held position past the end clamps", scroller{top: 95, follow: false}, 0, 100, 10, 90, 100},
		{"held position before first clamps", scroller{top: 5, follow: false}, 50, 100, 10, 50, 60},
		{"zero height shows nothing", scroller{top: 0, follow: true}, 0, 100, 0, 100, 100},
		{"empty log", scroller{top: 0, follow: true}, 0, 0, 10, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, stop := tt.s.window(tt.first, tt.end, tt.height)
			assert.Equal(t, tt.start, start, "start")
			assert.Equal(t, tt.stop, stop, "stop")
		})
	}
}

func TestScrollerScroll(t *testing.T) {
	tests := []struct {
		name   string
		s      scroller
		delta  int
		end    int
		top    int
		follow bool
	}{
		{"up from the tail stops following", newScroller(), -3, 100, 87, false},
		{"up past the start clamps", scroller{top: 2, follow: false}, -10, 100, 0, false},
		{"down within the log keeps holding", scroller{top: 10, follow: false}, 5, 100, 15, false},
		{"down to the bottom follows again", scroller{top: 85, follow: false}, 10, 100, 90, true},
		{"up in a log that fits keeps following", newScroller(), -1, 5, 0, true},
		{"down while following stays following", newScroller(), 1, 100, 90, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.scroll(tt.delta, 0, tt.end, 10)
			assert.Equal(t, tt.top, got.top, "top")
			assert.Equal(t, tt.follow, got.follow, "follow")
		})
	}
}

func TestScrollerHeldPositionSurvivesNewLines(t *testing.T) {
	s := newScroller().scroll(-5, 0, 100, 10)
	start, _ := s.window(0, 100, 10)
	assert.Equal(t, 85, start)
	start, _ = s.window(0, 500, 10)
	assert.Equal(t, 85, start, "new lines do not move a held window")
}

func TestScrollerJumpsAndFollowToggle(t *testing.T) {
	s := newScroller().toTop(0, 100, 10)
	assert.Equal(t, scroller{top: 0, follow: false}, s)
	assert.True(t, s.toBottom().follow)

	assert.Equal(t, scroller{top: 0, follow: true}, newScroller().toTop(0, 5, 10), "a log that fits keeps following")

	off := newScroller().toggleFollow(0, 100, 10)
	assert.Equal(t, scroller{top: 90, follow: false}, off, "turning follow off holds the current window")
	assert.True(t, off.toggleFollow(0, 100, 10).follow)
}
