package autoscroll

import (
	"testing"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestWindowScrollUpUnbottomsImmediately(t *testing.T) {
	if !gtk.InitCheck() {
		return
	}

	w := NewWindow()
	vadj := w.VAdjustment()
	vadj.Configure(900, 0, 1000, 10, 100, 100)

	w.ScrollToBottom()
	if !w.IsBottomed() {
		t.Fatalf("expected window to be bottomed after ScrollToBottom()")
	}

	// Simulate streaming growth while bottomed: upper grows to 1100.
	vadj.SetUpper(1100)
	if !w.IsBottomed() {
		t.Fatalf("expected window to remain bottomed when upper grows")
	}

	// Simulate the user scrolling upward (e.g. from 900 to 500) before idle callback runs.
	vadj.SetValue(500)
	if w.IsBottomed() {
		t.Fatalf("expected window to unbottom immediately when user scrolls upward")
	}

	// Subsequent ScrollToBottomIfBottomed() or upper growth must NOT pull the user back down.
	vadj.SetUpper(1200)
	w.ScrollToBottomIfBottomed()
	if got := vadj.Value(); got != 500 {
		t.Fatalf("expected scroll value to stay at 500 after scrolling up, got %v", got)
	}

	// Scrolling back to the bottom re-arms bottomed state.
	vadj.SetValue(1100)
	if !w.IsBottomed() {
		t.Fatalf("expected window to re-enter bottomed state when scrolled back to bottom")
	}
}
