package autoscroll

import (
	"log/slog"
	"math"

	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

type scrollState uint8

const (
	_ scrollState = iota
	bottomed
	locked
)

func (s scrollState) is(this scrollState) bool { return s == this }

// bottomEpsilon is the tolerance in pixels for considering the viewport at the bottom.
const bottomEpsilon = 2.0

// Window describes an automatically scrolled window that sticks to the bottom
// while bottomed, and stops auto-scrolling immediately when the user scrolls up.
type Window struct {
	*gtk.ScrolledWindow
	view   *gtk.Viewport
	vadj   *gtk.Adjustment
	logger *slog.Logger

	onBottomed func()

	upperValue          float64
	pageSize            float64
	lastValue           float64
	targetScroll        float64
	settingProgrammatic bool
	state               scrollState
}

// NewWindow creates a new auto-scrolling window.
func NewWindow() *Window {
	w := Window{
		upperValue:   math.NaN(),
		pageSize:     math.NaN(),
		lastValue:    math.NaN(),
		targetScroll: math.NaN(),
		state:        bottomed,
		logger:       slog.Default().With("widget", "autoscroll.Window"),
	}

	w.ScrolledWindow = gtk.NewScrolledWindow()
	w.SetPropagateNaturalHeight(true)
	w.SetPlacement(gtk.CornerBottomLeft)

	w.vadj = w.ScrolledWindow.VAdjustment()

	w.view = gtk.NewViewport(nil, w.vadj)
	w.view.SetVScrollPolicy(gtk.ScrollNatural)
	w.view.SetScrollToFocus(false)
	w.SetChild(w.view)

	w.ConnectMap(func() {
		if w.state.is(bottomed) {
			w.scrollTo(w.vadj.Upper()-w.vadj.PageSize(), false)
		} else if !math.IsNaN(w.targetScroll) {
			w.scrollTo(w.targetScroll, false)
		}
	})

	w.vadj.ConnectChanged(func() {
		w.upperValue = w.vadj.Upper()
		w.pageSize = w.vadj.PageSize()

		if w.state.is(bottomed) {
			newValue := max(0, w.upperValue-w.pageSize)
			w.scrollTo(newValue, true)
		}
	})

	w.vadj.ConnectValueChanged(func() {
		val := w.vadj.Value()
		prevVal := w.lastValue
		w.lastValue = val

		if w.state.is(locked) || w.settingProgrammatic {
			return
		}

		upper := w.vadj.Upper()
		pageSize := w.vadj.PageSize()
		bottomValue := upper - pageSize
		if bottomValue <= 0 || val >= bottomValue-bottomEpsilon {
			w.targetScroll = math.NaN()
			w.state = bottomed
			w.emitBottomed()
			return
		}

		// If the scroll value decreased (the user scrolled upward), immediately
		// unstick from the bottom and cancel any pending deferred auto-scroll
		// so continuous streaming updates cannot pull the viewport back down.
		if !math.IsNaN(prevVal) && val < prevVal-0.5 {
			w.targetScroll = math.NaN()
			w.state = 0
			return
		}

		// Otherwise, value < bottomValue may be transient because upper just
		// grew and our deferred scrollTo hasn't run yet. Verify on the next
		// idle frame whether we are still away from the bottom with no pending
		// programmatic target.
		glib.IdleAdd(func() {
			if w.state.is(locked) {
				return
			}
			curBottom := w.vadj.Upper() - w.vadj.PageSize()
			if curBottom <= 0 || w.vadj.Value() >= curBottom-bottomEpsilon {
				w.state = bottomed
				return
			}
			if math.IsNaN(w.targetScroll) {
				w.state = 0
			}
		})
	})

	return &w
}

// Viewport returns the ScrolledWindow's Viewport.
func (w *Window) Viewport() *gtk.Viewport {
	return w.view
}

// VAdjustment overrides gtk.ScrolledWindow's.
func (w *Window) VAdjustment() *gtk.Adjustment {
	return w.vadj
}

// LockScroll locks the scroll to the current value, even if more content is
// added. The returned function unlocks the scroll.
func (w *Window) LockScroll() func() {
	wasBottomed := w.state.is(bottomed)
	w.state = locked
	w.targetScroll = math.NaN()

	old := getScrollAdjustments(w.vadj)

	return func() {
		cur := getScrollAdjustments(w.vadj)
		value := cur.upper - old.upper + old.value

		if wasBottomed {
			w.state = bottomed
		} else {
			w.state = 0
		}
		w.scrollTo(value, true)
	}
}

// Unbottom clears the bottomed state.
func (w *Window) Unbottom() {
	if w.state.is(bottomed) {
		w.state = 0
		w.targetScroll = math.NaN()
	}
}

// IsBottomed returns true if the scrolled window is currently bottomed out.
func (w *Window) IsBottomed() bool {
	return w.state.is(bottomed)
}

// ScrollToBottom scrolls the window to bottom unconditionally (e.g. on initial
// load or when the user sends a prompt).
func (w *Window) ScrollToBottom() {
	w.state = bottomed
	w.scrollTo(max(0, w.vadj.Upper()-w.vadj.PageSize()), false)
}

// ScrollToBottomIfBottomed scrolls the window to the bottom only if it is
// currently in the bottomed state.
func (w *Window) ScrollToBottomIfBottomed() {
	if w.state.is(bottomed) {
		w.scrollTo(max(0, w.vadj.Upper()-w.vadj.PageSize()), false)
	}
}

// OnBottomed registers the given function to be called when the user bottoms
// out the scrolled window.
func (w *Window) OnBottomed(f func()) {
	if w.onBottomed == nil {
		w.onBottomed = f
		return
	}

	old := w.onBottomed
	w.onBottomed = func() {
		old()
		f()
	}
}

func (w *Window) emitBottomed() {
	if w.onBottomed != nil {
		w.onBottomed()
	}
}

// SetChild sets the child of the ScrolledWindow.
func (w *Window) SetChild(child gtk.Widgetter) {
	_, scrollable := child.(gtk.Scrollabler)
	if scrollable {
		w.ScrolledWindow.SetChild(child)
	} else {
		w.view.SetChild(child)
		w.ScrolledWindow.SetChild(w.view)
	}
}

func (w *Window) scrollTo(targetScroll float64, deferFn bool) {
	w.targetScroll = targetScroll
	previousAdjs := getScrollAdjustments(w.vadj)

	doScroll := func() {
		if w.targetScroll != targetScroll {
			return
		}
		// If we were unbottomed while waiting for the idle callback, abort.
		if deferFn && w.state == 0 {
			w.targetScroll = math.NaN()
			return
		}

		currentAdjs := getScrollAdjustments(w.vadj)
		actualTarget := targetScroll
		if w.state.is(bottomed) {
			actualTarget = max(0, currentAdjs.upper-w.vadj.PageSize())
		} else if currentAdjs.upper != previousAdjs.upper {
			actualTarget += currentAdjs.upper - previousAdjs.upper
		}

		w.targetScroll = math.NaN()
		w.settingProgrammatic = true
		w.vadj.SetValue(actualTarget)
		w.settingProgrammatic = false
	}

	if deferFn {
		glib.IdleAdd(doScroll)
	} else {
		doScroll()
	}
}

type scrollAdjustments struct {
	lower float64
	upper float64
	value float64
}

func getScrollAdjustments(adj *gtk.Adjustment) scrollAdjustments {
	return scrollAdjustments{
		lower: adj.Lower(),
		upper: adj.Upper(),
		value: adj.Value(),
	}
}
