// Package progress renders the import pipeline's two long-lived workers — the
// Fetch (producer/network) goroutine and the Process (consumer/DB) goroutine —
// as two persistent progress bars, so an operator can see at a glance how far
// each is, its rate/ETA, and which one is the current bottleneck.
//
// There are exactly two lanes, Fetch() and Process(), each a singleton that
// lives for the whole session. A lane with no current work stays on screen,
// full, labeled "idle"; Start/Add/SetTotal/Done drive it through a unit of work
// (one resource's fetch, or one resource's typing pass). Both can be active at
// once (the pipelined init path) or one-at-a-time (the default
// fetch-then-process path) — either way both bars are always present.
//
// Bars are drawn with mpb when stdout is an interactive terminal (or
// MLS_SYNC_PROGRESS=always). When piped/redirected/CI (or =never), mpb would
// refresh-spam, so the lanes fall back to throttled plain log lines. Log()
// routes ordinary app logging either above the bars (mpb's Write, which is the
// only safe way to print while bars are live) or to the underlying logger.
//
// Concurrency / deadlock note: mpb's render goroutine invokes the decor
// callbacks, and the lane mutators call mpb bar setters that send on mpb's
// channels. Those two must never share a lock, or the renderer (waiting on the
// lock) and the setter (waiting on the renderer to drain) deadlock. So a lane's
// display state lives in atomics: the decor callbacks read atomics and touch
// neither a lock nor mpb, while the mutators update atomics and then call the
// mpb setter outside any lock.
package progress

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lunarhue/libs-go/log"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

// Mode selects how a session renders. Auto picks bars when stdout is an
// interactive terminal and plain lines otherwise; Never/Always force it.
type Mode int

const (
	ModeAuto Mode = iota
	ModeNever
	ModeAlways
)

// ParseMode maps a config string ("auto"/"never"/"always") to a Mode,
// defaulting to Auto for anything unrecognized.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "never", "off", "false", "no":
		return ModeNever
	case "always", "force", "yes":
		return ModeAlways
	default:
		return ModeAuto
	}
}

// Level is the severity of a Log line.
type Level int

const (
	LevelInfo Level = iota
	LevelError
	LevelDebug
)

const (
	barWidth       = 24  // inner bar cells
	maxLineWidth   = 100 // cap the whole rendered line so it never wraps
	minRateElapsed = 0.5 // seconds before a rate/ETA is meaningful enough to show
)

// Throttles bound output frequency. Vars (not consts) so tests can zero them.
var lineThrottle = 2 * time.Second // min gap between non-TTY heartbeat lines

// Lane is one of the two persistent workers. Start begins a unit of work
// (relabel, reset, set total — <=0 means unknown, filled in later by SetTotal),
// Add advances it, and Done finishes it (the lane goes idle: full bar + "idle").
type Lane interface {
	Start(label string, total int)
	Add(n int)
	SetTotal(n int)
	Done()
}

// overridable in tests
var (
	stdout     io.Writer = os.Stdout
	isTerminal           = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	emitLine             = func(s string) { fmt.Fprintln(stdout, s) }
	termWidth            = func() int {
		w, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return 0
		}
		return w
	}
)

var (
	fetchLane = &lane{short: "Fetch"}
	procLane  = &lane{short: "Process"}

	sessMu sync.Mutex    // guards prog/barsOn across Begin/End vs Log
	prog   *mpb.Progress // non-nil only in bar mode
	barsOn bool          // true when bars are live
	logMu  sync.Mutex    // serializes the non-bar libs-go logging path
)

// Fetch returns the producer lane; Process the consumer lane. Both are valid
// even with no active session (they fall back to plain lines / no-ops).
func Fetch() Lane   { return fetchLane }
func Process() Lane { return procLane }

// laneState is the textual display state read by the decor callbacks. Stored
// behind an atomic.Pointer so the callbacks never lock.
type laneState struct {
	label string
	idle  bool
}

type lane struct {
	short string // "Fetch" / "Process" — fixed prefix for labels and lines

	cur   atomic.Int64
	total atomic.Int64 // <=0 unknown
	start atomic.Int64 // UnixNano of the current task's start (for rate/ETA)
	state atomic.Pointer[laneState]

	bar *mpb.Bar // nil unless bar mode

	mu      sync.Mutex // guards lastLog (non-TTY throttle only)
	lastLog time.Time
}

// Begin opens the process-wide progress session. Safe to call when one is
// already open. Pair with End.
func Begin(mode Mode) {
	sessMu.Lock()
	defer sessMu.Unlock()

	bars := mode == ModeAlways || (mode == ModeAuto && isTerminal())
	for _, l := range []*lane{fetchLane, procLane} {
		l.cur.Store(0)
		l.total.Store(0)
		l.start.Store(0)
		l.state.Store(&laneState{}) // label "", idle → renders "<short> — idle"
		l.bar = nil
		l.lastLog = time.Time{}
	}

	if !bars {
		prog, barsOn = nil, false
		return
	}

	// WithAutoRefresh makes mpb render on a timer even when the output isn't a
	// detected terminal — needed for MLS_SYNC_PROGRESS=always (forced bars on a
	// non-TTY) and harmless on a real terminal (which already auto-refreshes).
	opts := []mpb.ContainerOption{mpb.WithOutput(stdout), mpb.WithAutoRefresh()}
	if w := termWidth(); w > 0 {
		opts = append(opts, mpb.WithWidth(min(w, maxLineWidth)))
	}
	prog = mpb.New(opts...)
	barsOn = true
	fetchLane.bar = newBar(prog, fetchLane)
	procLane.bar = newBar(prog, procLane)
}

func newBar(p *mpb.Progress, l *lane) *mpb.Bar {
	style := mpb.BarStyle().Lbound("▕").Filler("█").Tip("█").Padding("░").Rbound("▏")
	return p.New(0, style,
		mpb.BarWidth(barWidth),
		mpb.PrependDecorators(
			decor.Any(func(decor.Statistics) string { return l.labelText() }, decor.WCSyncSpaceR),
		),
		mpb.AppendDecorators(
			decor.Any(func(decor.Statistics) string { return l.statusText() }, decor.WCSyncSpace),
		),
	)
}

// End closes the session. In bar mode it completes both bars (so mpb's Wait
// returns) and flushes the final frame.
func End() {
	sessMu.Lock()
	p, bars := prog, barsOn
	prog, barsOn = nil, false
	sessMu.Unlock()

	if !bars || p == nil {
		return
	}
	for _, l := range []*lane{fetchLane, procLane} {
		if l.bar == nil {
			continue
		}
		t := l.total.Load()
		if t <= 0 {
			t = max(l.cur.Load(), 1)
		}
		l.cur.Store(t)
		l.bar.SetCurrent(t)
		l.bar.SetTotal(t, true) // trigger complete so Wait() returns
		l.bar = nil
	}
	p.Wait()
}

// Log routes one log line: above the live bars (mpb Write) in bar mode, or
// through the underlying logger otherwise. DEBUG is dropped below debug level.
func Log(level Level, msg string) {
	sessMu.Lock()
	p, bars := prog, barsOn
	sessMu.Unlock()

	if bars && p != nil {
		if level == LevelDebug && log.GetLevel() < log.DEBUG {
			return
		}
		fmt.Fprintln(p, formatLogLine(level, msg))
		return
	}

	logMu.Lock()
	defer logMu.Unlock()
	switch level {
	case LevelError:
		log.Errorf("%s", msg)
	case LevelDebug:
		log.Debugf("%s", msg)
	default:
		log.Infof("%s", msg)
	}
}

// --- Lane implementation ---

func (l *lane) Start(label string, total int) {
	l.start.Store(time.Now().UnixNano())
	l.cur.Store(0)
	if total > 0 {
		l.total.Store(int64(total))
	} else {
		l.total.Store(0)
	}
	l.state.Store(&laneState{label: label, idle: false})
	l.mu.Lock()
	l.lastLog = time.Time{}
	l.mu.Unlock()

	if l.bar != nil {
		l.bar.SetCurrent(0)
		l.bar.SetTotal(int64(max(total, 1)), false) // 1 placeholder until SetTotal lands
	}
}

func (l *lane) Add(n int) {
	cur := l.cur.Add(int64(n))
	if l.bar != nil {
		l.bar.SetCurrent(cur)
		return
	}
	l.maybeLine()
}

func (l *lane) SetTotal(n int) {
	if n <= 0 {
		return
	}
	for {
		old := l.total.Load()
		if int64(n) <= old {
			break
		}
		if l.total.CompareAndSwap(old, int64(n)) {
			break
		}
	}
	if l.bar != nil {
		l.bar.SetTotal(l.total.Load(), false)
	}
}

func (l *lane) Done() {
	total := l.total.Load()
	if total <= 0 {
		total = max(l.cur.Load(), 1)
		l.total.Store(total)
	}
	l.cur.Store(total)
	st := l.state.Load()
	l.state.Store(&laneState{label: st.label, idle: true}) // keep label, go idle/full

	if l.bar != nil {
		l.bar.SetTotal(total, false)
		l.bar.SetCurrent(total)
	}
}

// maybeLine emits a throttled plain heartbeat line (non-TTY mode only).
func (l *lane) maybeLine() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.lastLog.IsZero() && now.Sub(l.lastLog) < lineThrottle {
		return
	}
	l.lastLog = now
	cur, total := l.cur.Load(), l.total.Load()
	elapsed := time.Since(time.Unix(0, l.start.Load())).Seconds()
	emitLine(formatProgress(l.name(), int(cur), int(total), rate(cur, elapsed)))
}

func (l *lane) name() string {
	label := l.state.Load().label
	if label == "" {
		label = "—"
	}
	return l.short + " " + label
}

// labelText / statusText are read by the mpb decor callbacks — atomics only,
// no locks, no mpb calls (see the package deadlock note).
func (l *lane) labelText() string { return l.name() }

func (l *lane) statusText() string {
	if l.state.Load().idle {
		return "idle"
	}
	cur, total := l.cur.Load(), l.total.Load()
	elapsed := time.Since(time.Unix(0, l.start.Load())).Seconds()
	return statusLine(cur, total, rate(cur, elapsed))
}

// --- formatting (shared by bar decorators and non-TTY lines) ---

// statusLine is the right-hand side of a bar: "52% 306k/589k 15.5k/s ETA 18s",
// or count + rate when the total is unknown.
func statusLine(cur, total int64, r float64) string {
	var b strings.Builder
	if total > 0 {
		fmt.Fprintf(&b, "%3.0f%% %s/%s", pctOf(cur, total), short(cur), short(total))
	} else {
		b.WriteString(short(cur))
	}
	if r > 0 {
		fmt.Fprintf(&b, " %s", rateStr(r))
		if total > cur {
			fmt.Fprintf(&b, " ETA %s", etaStr(etaDur(cur, total, r)))
		}
	}
	return b.String()
}

// formatProgress is the non-TTY heartbeat line:
//
//	"Process property: 306,000/589,081 (52%) — 15.5k/s, ETA 18s"
//	"Fetch Property: 1,234 — 8.1k/s"   (unknown total)
func formatProgress(name string, cur, total int, r float64) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString(": ")
	if total > 0 {
		fmt.Fprintf(&b, "%s/%s (%.0f%%)", comma(cur), comma(total), pctOf(int64(cur), int64(total)))
	} else {
		b.WriteString(comma(cur))
	}
	if r > 0 {
		fmt.Fprintf(&b, " — %s", rateStr(r))
		if total > cur {
			fmt.Fprintf(&b, ", ETA %s", etaStr(etaDur(int64(cur), int64(total), r)))
		}
	}
	return b.String()
}

func formatLogLine(level Level, msg string) string {
	const gray, reset = "\033[90m", "\033[0m"
	lvl, col := "INFO", "\033[34m"
	switch level {
	case LevelError:
		lvl, col = "ERROR", "\033[31m"
	case LevelDebug:
		lvl, col = "DEBUG", "\033[36m"
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("%s%s%s %s%s%s: %s", gray, ts, reset, col, lvl, reset, msg)
}

func rate(cur int64, elapsed float64) float64 {
	if elapsed < minRateElapsed {
		return 0
	}
	return float64(cur) / elapsed
}

func pctOf(cur, total int64) float64 {
	if total <= 0 {
		return 0
	}
	if p := float64(cur) / float64(total) * 100; p < 100 {
		return p
	}
	return 100
}

func etaDur(cur, total int64, r float64) time.Duration {
	return time.Duration(float64(total-cur) / r * float64(time.Second))
}

// comma renders 589081 as "589,081".
func comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// short renders a compact count for the bar: 306000→"306k", 1500000→"1.5M".
func short(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return strconv.FormatInt(n/1000, 10) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// rateStr renders an items/sec rate: 15495.9→"15.5k/s", 840→"840/s".
func rateStr(r float64) string {
	if r >= 1000 {
		return fmt.Sprintf("%.1fk/s", r/1000)
	}
	return fmt.Sprintf("%.0f/s", r)
}

// etaStr renders a remaining duration to whole seconds: "18s", "1m30s".
func etaStr(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
