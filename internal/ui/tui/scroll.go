package tui

// scroller is a vertical position in a growing log. While following, the
// window sticks to the newest lines. Otherwise it holds top, an absolute
// line index, so the view stays put as lines arrive or old ones fall off
// the buffer.
type scroller struct {
	top    int
	follow bool
}

func newScroller() scroller { return scroller{top: 0, follow: true} }

// maxTop is the highest top that still fills the window.
func maxTop(first, end, height int) int {
	return max(first, end-max(1, height))
}

// window returns the absolute range [start, stop) of lines to show for a
// buffer holding [first, end) in height rows.
func (s scroller) window(first, end, height int) (int, int) {
	if height <= 0 {
		return end, end
	}
	start := maxTop(first, end, height)
	if !s.follow {
		start = min(max(s.top, first), start)
	}
	return start, min(end, start+height)
}

// scroll moves the window by delta lines. Scrolling up stops following;
// reaching the bottom while scrolling down starts it again.
func (s scroller) scroll(delta, first, end, height int) scroller {
	start, _ := s.window(first, end, height)
	limit := maxTop(first, end, height)
	top := min(max(start+delta, first), limit)
	switch {
	case delta < 0 && top < limit:
		return scroller{top: top, follow: false}
	case delta > 0 && top >= limit:
		return scroller{top: top, follow: true}
	default:
		return scroller{top: top, follow: s.follow}
	}
}

// toTop jumps to the oldest line kept.
func (s scroller) toTop(first, end, height int) scroller {
	return scroller{top: first, follow: maxTop(first, end, height) == first && s.follow}
}

// toBottom jumps to the newest line and follows.
func (s scroller) toBottom() scroller {
	return scroller{top: s.top, follow: true}
}

// toggleFollow switches following. Turning it off keeps the lines in view
// where they are.
func (s scroller) toggleFollow(first, end, height int) scroller {
	if s.follow {
		start, _ := s.window(first, end, height)
		return scroller{top: start, follow: false}
	}
	return scroller{top: s.top, follow: true}
}
