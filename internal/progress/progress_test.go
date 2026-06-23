package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{
		"auto": ModeAuto, "": ModeAuto, "weird": ModeAuto,
		"never": ModeNever, "off": ModeNever, "FALSE": ModeNever,
		"always": ModeAlways, "force": ModeAlways, "  Always ": ModeAlways,
	} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFormatProgress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cur, total int
		rate       float64
		want       string
	}{
		{
			name: "known total with rate and ETA",
			cur:  306000, total: 589081, rate: 15495.9,
			want: "Process property: 306,000/589,081 (52%) — 15.5k/s, ETA 18s",
		},
		{
			name: "unknown total shows count and rate only",
			cur:  1234, total: -1, rate: 8100,
			want: "Process property: 1,234 — 8.1k/s",
		},
		{
			name: "complete: no ETA once cur == total",
			cur:  40, total: 40, rate: 1560,
			want: "Process property: 40/40 (100%) — 1.6k/s",
		},
		{
			name: "no rate yet (just started)",
			cur:  0, total: 100, rate: 0,
			want: "Process property: 0/100 (0%)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatProgress("Process property", tc.cur, tc.total, tc.rate); got != tc.want {
				t.Errorf("formatProgress:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestStatusLine(t *testing.T) {
	got := statusLine(306000, 589081, 15495.9)
	for _, want := range []string{"52%", "306k/589k", "15.5k/s", "ETA 18s"} {
		if !strings.Contains(got, want) {
			t.Errorf("statusLine %q missing %q", got, want)
		}
	}
	// Unknown total: count + rate, no percent or ETA.
	u := statusLine(1234, -1, 8100)
	if strings.Contains(u, "%") || strings.Contains(u, "ETA") {
		t.Errorf("unknown-total status should be count+rate only: %q", u)
	}
}

func TestNumberFormatHelpers(t *testing.T) {
	for in, want := range map[int]string{0: "0", 1234: "1,234", 589081: "589,081", -589081: "-589,081"} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{500: "500", 306000: "306k", 1500000: "1.5M"} {
		if got := short(in); got != want {
			t.Errorf("short(%d) = %q, want %q", in, got, want)
		}
	}
	if got := rateStr(15495.9); got != "15.5k/s" {
		t.Errorf("rateStr = %q", got)
	}
	if got := rateStr(840); got != "840/s" {
		t.Errorf("rateStr = %q", got)
	}
	if got := etaStr(90 * time.Second); got != "1m30s" {
		t.Errorf("etaStr = %q", got)
	}
}

// TestNonTTYLaneEmitsThrottledLine drives a lane in plain-line (non-bar) mode
// and asserts it emits a formatted line — count-only first, then a percentage
// once a late SetTotal (the $count from page 1) lands.
func TestNonTTYLaneEmitsThrottledLine(t *testing.T) {
	var lines []string
	restore := emitLine
	emitLine = func(s string) { lines = append(lines, s) }
	throttle := lineThrottle
	lineThrottle = 0 // emit every update so we can observe the progression
	defer func() { emitLine = restore; lineThrottle = throttle }()

	Begin(ModeNever) // force line mode regardless of the test's stdout
	defer End()

	fetch := Fetch()
	fetch.Start("Property", -1)
	fetch.Add(100) // unknown total → count-only line
	fetch.SetTotal(1000)
	fetch.Add(400) // now 500/1000

	if len(lines) == 0 {
		t.Fatal("expected at least one heartbeat line")
	}
	if !strings.HasPrefix(lines[0], "Fetch Property: 100") || strings.Contains(lines[0], "/") {
		t.Errorf("first line should be count-only (no total): %q", lines[0])
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "500/1,000 (50%)") {
		t.Errorf("last line should carry the percentage: %q", last)
	}
}

// TestNonTTYIdleNoPanic exercises Start→Add→Done and the not-started idle state
// in line mode (Done is a no-op there; the caller logs the summary).
func TestNonTTYIdleNoPanic(t *testing.T) {
	Begin(ModeNever)
	defer End()
	p := Process()
	p.Start("property", 10)
	p.Add(10)
	p.Done() // → idle, no panic
}

// TestBarModeConcurrentNoDeadlock is the load-bearing race/deadlock guard for
// the mpb path: the render goroutine runs the decor callbacks while many
// goroutines mutate both lanes and Log above the bars. A shared lock between
// the decor reads and the bar setters would hang here (run under -race).
func TestBarModeConcurrentNoDeadlock(t *testing.T) {
	var buf bytes.Buffer
	restoreOut, restoreTW := stdout, termWidth
	stdout = &buf
	termWidth = func() int { return 80 }
	defer func() { stdout, termWidth = restoreOut, restoreTW }()

	Begin(ModeAlways)

	f, p := Fetch(), Process()
	f.Start("Property", 1000)
	p.Start("property", 1000)

	var wg sync.WaitGroup
	for _, lane := range []Lane{f, p} {
		wg.Add(1)
		go func(l Lane) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Add(5)
			}
		}(lane)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			Log(LevelInfo, "interleaved log line")
		}
	}()
	wg.Wait()

	f.Done()
	p.Done()
	End() // completes bars + flushes; must not hang

	if buf.Len() == 0 {
		t.Error("bar mode produced no output")
	}
}
