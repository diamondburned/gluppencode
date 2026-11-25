package mdstream

import (
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	"weak"

	glibv2 "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func pumpGLib(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for glibv2.MainContextDefault().Pending() {
			glibv2.MainContextDefault().Iteration(false)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for glibv2.MainContextDefault().Pending() {
		glibv2.MainContextDefault().Iteration(false)
	}
}

func waitForWeakReclaim[T any](t *testing.T, label string, ptrs []weak.Pointer[T], baseGoroutines int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for glibv2.MainContextDefault().Pending() {
			glibv2.MainContextDefault().Iteration(false)
		}
		runtime.GC()
		debug.FreeOSMemory()
		// Yield briefly so finalizers and gotk4 cleanup callbacks run.
		time.Sleep(15 * time.Millisecond)
		for glibv2.MainContextDefault().Pending() {
			glibv2.MainContextDefault().Iteration(false)
		}
		runtime.GC()

		alive := 0
		for _, wp := range ptrs {
			if wp.Value() != nil {
				alive++
			}
		}
		if alive == 0 && runtime.NumGoroutine() <= baseGoroutines {
			return
		}
	}

	alive := 0
	for _, wp := range ptrs {
		if wp.Value() != nil {
			alive++
		}
	}
	t.Fatalf("%s: after GC+GLib drain, %d/%d objects still retained; goroutines=%d (baseline=%d)",
		label, alive, len(ptrs), runtime.NumGoroutine(), baseGoroutines)
}

func TestViewReclaimsMemoryAndGoroutines(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	// Keep block count moderate (~30 blocks/view) while keeping byte size substantial
	// (~32 KB/view) so HeapAlloc growth & drop are clear without creating 20k+ widgets.
	longPara := strings.Repeat("markdown streaming memory test words with **bold** and `code` ", 40)
	payload := strings.Repeat("# Heading\n\n"+longPara+"\n\n```go\npackage main\nfunc main() { println(\"hello\") }\n```\n\n", 10)

	cases := []struct {
		name     string
		teardown func(parent *gtk.Box, views []*View)
	}{
		{
			name: "finalize then unparent",
			teardown: func(parent *gtk.Box, views []*View) {
				for _, v := range views {
					v.Finalize("")
					parent.Remove(v)
				}
			},
		},
		{
			name: "close mid-stream then unparent",
			teardown: func(parent *gtk.Box, views []*View) {
				for _, v := range views {
					v.Close()
					parent.Remove(v)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const count = 40

			// Warm up once and settle baseline goroutines/heap before measuring.
			pumpGLib(20 * time.Millisecond)
			runtime.GC()
			var baseStats runtime.MemStats
			runtime.ReadMemStats(&baseStats)
			baseGoroutines := runtime.NumGoroutine()

			parent := gtk.NewBox(gtk.OrientationVertical, 0)

			var viewRefs []weak.Pointer[View]
			var engineRefs []weak.Pointer[Engine]
			var liveHeap uint64

			func() {
				ctx := context.Background()
				views := make([]*View, 0, count)
				for range count {
					v := NewView(ctx)
					parent.Append(v)
					v.AppendDelta(payload[:len(payload)/2])
					v.AppendDelta(payload[len(payload)/2:])

					v.mu.Lock()
					eng := v.engine
					v.mu.Unlock()

					viewRefs = append(viewRefs, weak.Make(v))
					if eng != nil {
						engineRefs = append(engineRefs, weak.Make(eng))
					}
					views = append(views, v)
				}

				pumpGLib(50 * time.Millisecond)
				runtime.GC()
				var liveStats runtime.MemStats
				runtime.ReadMemStats(&liveStats)
				liveHeap = liveStats.HeapAlloc

				tc.teardown(parent, views)
				clear(views)
				runtime.KeepAlive(views)
			}()

			// Drain pending streamer patches / destroy signals.
			pumpGLib(80 * time.Millisecond)

			waitForWeakReclaim(t, tc.name+" (Engine)", engineRefs, baseGoroutines)
			waitForWeakReclaim(t, tc.name+" (View)", viewRefs, baseGoroutines)

			var afterStats runtime.MemStats
			runtime.ReadMemStats(&afterStats)
			if liveHeap > baseStats.HeapAlloc+512*1024 {
				retained := int64(afterStats.HeapAlloc) - int64(baseStats.HeapAlloc)
				peakDelta := int64(liveHeap) - int64(baseStats.HeapAlloc)
				if retained > peakDelta/2 {
					t.Fatalf("%s: heap did not drop after reclaiming %d Views: base=%d live=%d after=%d",
						tc.name, count, baseStats.HeapAlloc, liveHeap, afterStats.HeapAlloc)
				}
			}

			runtime.KeepAlive(parent)
		})
	}
}
