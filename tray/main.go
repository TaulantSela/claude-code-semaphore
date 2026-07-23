// claude-semaphore — a cross-platform system tray traffic light for Claude Code.
//
// Reads per-session state files written by the plugin's hook script into
// ~/.claude/semaphore/ and shows the aggregate state:
//
//	red    — Claude is waiting for your input (permission prompt or question)
//	orange — Claude is working, or a session is idle
//	green  — task finished
//	gray   — no active Claude sessions
//
// Aggregation: any red session wins; otherwise the most recently active
// session speaks, so a stale idle session in another window cannot mask a
// freshly finished task.
//
// The working (orange) and needs-input (red) states are ANIMATED:
//
//	orange — breathes (opacity) to signal "something is happening", but only
//	         while a session was touched recently; a walked-away session
//	         holds a steady bright dot instead of breathing all day.
//	red    — beats like a heart (a two-beat pulse with a soft glow bloom) to
//	         draw the eye without alarm, and keeps beating until you respond
//	         and the state clears.
//
// Both animations cycle a small, FIXED set of PNG frames pre-rendered once at
// startup — never frames derived from a live clock — because systray's Windows
// and Linux backends cache one icon handle per distinct payload and never free
// it, so an open-ended set of frames would leak handles in a process that runs
// for days.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"
)

const (
	// Binding this port is the cross-platform single-instance lock: a second
	// copy fails to bind and exits silently, so the bootstrap hook can spawn
	// the app unconditionally.
	singleInstanceAddr = "127.0.0.1:47816"

	// Sessions that crashed without a SessionEnd hook leave files behind;
	// ignore anything untouched for this long.
	staleAfter = 12 * time.Hour

	// How often to re-read the state directory.
	pollInterval = time.Second

	// How often to advance an animation frame (20 fps). Fast enough that the red
	// dot's vertical jump reads as smooth motion rather than a stutter; still
	// under the rate at which the Linux DBus backend and the Windows taskbar
	// start to complain.
	frameInterval = 50 * time.Millisecond

	// Orange breathes only while the newest session file was touched within
	// this window. Active work touches it far more often (every prompt and
	// tool call); a session left mid-work goes quietly static instead of
	// breathing on the menu bar — and burning CPU — until the 12h sweep.
	activeWindow = 3 * time.Minute
)

var stateDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "semaphore")
}()

type status struct {
	state    string // "red" | "orange" | "green" | "idle"
	sessions int
}

var labels = map[string]string{
	"red":    "Claude needs your input",
	"orange": "Claude is working…",
	"green":  "Task finished",
	"idle":   "No active Claude sessions",
}

// Non-premultiplied colours, tuned vibrant so they read clearly at menu-bar
// size. circlePNG renders into an NRGBA image, so these stay true at any alpha
// — a premultiplied RGBA image would turn a half-transparent orange into dark
// green. Idle stays a muted grey on purpose; it should not draw the eye.
var stateColor = map[string]color.NRGBA{
	"red":    {R: 0xFF, G: 0x10, B: 0x10, A: 0xFF}, // near-pure vivid red
	"orange": {R: 0xFF, G: 0x8A, B: 0x00, A: 0xFF}, // hot vivid orange
	"green":  {R: 0x0A, G: 0xE2, B: 0x54, A: 0xFF}, // hot vivid green
	"idle":   {R: 0x9E, G: 0x9E, B: 0x9E, A: 0xFF},
}

func main() {
	lock, err := net.Listen("tcp", singleInstanceAddr)
	if err != nil {
		os.Exit(0) // another instance is already running
	}
	defer lock.Close()

	_ = os.MkdirAll(stateDir, 0o755)
	systray.Run(onReady, func() {})
}

func onReady() {
	buildFrames()

	// macOS belt-and-suspenders: a non-empty title forces the status button
	// into NSImageLeft, the colour-preserving image mode, so the coloured dot
	// survives even on macOS versions where an icon-only item renders as a
	// monochrome mask. A single space shows no glyph — just the coloured dot.
	// SetTitle is a no-op on Windows and tooltip-only on Linux, so this is
	// harmless there.
	if runtime.GOOS == "darwin" {
		systray.SetTitle(" ")
	}

	statusItem := systray.AddMenuItem(labels["idle"], "")
	statusItem.Disable()
	systray.AddSeparator()
	resetItem := systray.AddMenuItem("Reset to idle", "Clear all session states")
	quitItem := systray.AddMenuItem("Quit", "")

	applyLabel := func(st status) {
		label := labels[st.state]
		if st.sessions > 1 {
			label = fmt.Sprintf("%s  (%d sessions)", label, st.sessions)
		}
		systray.SetTooltip(label)
		statusItem.SetTitle(label)
	}

	cur, newest := readStatus()
	frame := 0

	// draw paints the current frame. Orange loops while fresh and holds a
	// bright static dot when stale; red loops its jump forever; the rest are a
	// single static frame.
	draw := func() {
		icons := frameIcons[cur.state]
		idx := 0
		switch cur.state {
		case "orange":
			if time.Since(newest) < activeWindow {
				idx = frame % len(icons)
			} // else idx 0 == fully bright: a steady, non-breathing dot
		case "red":
			idx = frame % len(icons)
		}
		systray.SetIcon(icons[idx])
	}

	// wantAnim reports whether the ticker should be running right now. Red
	// keeps jumping the whole time it is red — until you respond and the hook
	// flips the state — so it never stops on its own.
	wantAnim := func() bool {
		switch cur.state {
		case "orange":
			return time.Since(newest) < activeWindow
		case "red":
			return true
		default:
			return false
		}
	}

	applyLabel(cur)
	draw()

	go func() {
		poll := time.NewTicker(pollInterval)
		defer poll.Stop()

		// The animation ticker exists only while something is actually
		// animating. When idle, animC is nil — a nil channel blocks forever in
		// select, so a static tray costs nothing between polls and there is no
		// per-transition goroutine to leak.
		var anim *time.Ticker
		var animC <-chan time.Time
		setAnim := func(on bool) {
			switch {
			case on && anim == nil:
				anim = time.NewTicker(frameInterval)
				animC = anim.C
			case !on && anim != nil:
				anim.Stop()
				anim = nil
				animC = nil
			}
		}
		setAnim(wantAnim())

		for {
			select {
			case <-poll.C:
				st, n := readStatus()
				if st.state != cur.state {
					frame = 0 // restart the new state's animation
				}
				changed := st != cur
				cur, newest = st, n
				if changed {
					applyLabel(cur)
					draw()
				}
				// Re-evaluated every poll so orange can start/stop breathing as
				// a session goes active or quiet, without a state change.
				setAnim(wantAnim())
			case <-animC:
				frame++
				draw()
				if !wantAnim() {
					setAnim(false) // orange went quiet: hold a steady dot, stop ticking
				}
			case <-resetItem.ClickedCh:
				entries, _ := os.ReadDir(stateDir)
				for _, e := range entries {
					_ = os.Remove(filepath.Join(stateDir, e.Name()))
				}
				cur, newest = status{state: "idle"}, time.Now()
				frame = 0
				applyLabel(cur)
				draw()
				setAnim(false)
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

// readStatus aggregates the per-session files and also returns the modification
// time of the most recently touched one, used to tell an actively-working
// orange session from an abandoned one.
func readStatus() (status, time.Time) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return status{state: "idle"}, time.Time{}
	}
	cutoff := time.Now().Add(-staleAfter)
	anyRed := false
	newest := time.Time{}
	newestState := ""
	sessions := 0

	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			continue
		}
		state := strings.TrimSpace(string(raw))
		if state != "red" && state != "orange" && state != "green" {
			continue
		}
		sessions++
		if state == "red" {
			anyRed = true
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
			newestState = state
		}
	}

	switch {
	case anyRed:
		return status{"red", sessions}, newest
	case newestState == "green":
		return status{"green", sessions}, newest
	case sessions > 0:
		return status{"orange", sessions}, newest
	default:
		return status{"idle", 0}, newest
	}
}

// --- animation frames ----------------------------------------------------

// frame describes one rendered still. All values are fractions so the same
// table renders at any icon size.
type frame struct {
	radiusFrac float64 // 1.0 = fills the icon (minus a 1px margin)
	alpha      float64 // 1.0 = opaque
	liftFrac   float64 // fraction of icon height to shift the disc upward
	flash      float64 // 0 = state colour, 1 = white (brightness flash)
	glow       float64 // 0 = none; a soft halo blooming beyond the disc rim
}

// frameIcons holds the encoded, ready-to-hand-to-systray bytes for every
// state — one entry for static states, a full sequence for animated ones.
// Built once in buildFrames() and read-only thereafter.
var frameIcons = map[string][][]byte{}

func buildFrames() {
	size := iconSize()
	reduce := motionReduced()
	win := runtime.GOOS == "windows"
	tables := map[string][]frame{
		"idle":   {{radiusFrac: 1.0, alpha: 1.0}},
		"green":  {{radiusFrac: 1.0, alpha: 1.0}},
		"orange": orangeFrames(reduce),
		"red":    redFrames(reduce),
	}
	for state, table := range tables {
		col := stateColor[state]
		icons := make([][]byte, len(table))
		for i, f := range table {
			b := circlePNG(col, size, f.radiusFrac, f.alpha, f.liftFrac, f.flash, f.glow)
			if win {
				b = pngToICO(b, size)
			}
			icons[i] = b
		}
		frameIcons[state] = icons
	}
}

// framesFor returns how many frames span a duration at the animation tick
// rate, so a cycle's wall-clock period stays fixed if the tick rate changes.
func framesFor(d time.Duration) int {
	n := int(d / frameInterval)
	if n < 2 {
		n = 2
	}
	return n
}

// orangeFrames is a calm opacity breathe on a size-steady disc. Breathing the
// radius instead would read as edge shimmer at menu-bar size; dimming does not.
// The floor stays at 0.6 so it never fades far enough to look like the app
// died, and frame 0 is fully bright so entering "working" — and the static
// stale-session dot — never begins mid-fade. One breath ≈ 2.6 s.
func orangeFrames(reduce bool) []frame {
	if reduce {
		return []frame{{radiusFrac: 1.0, alpha: 1.0}}
	}
	n := framesFor(1400 * time.Millisecond)
	fs := make([]frame, n)
	for i := 0; i < n; i++ {
		bright := 0.5 + 0.5*math.Cos(2*math.Pi*float64(i)/float64(n)) // 1→0→1
		fs[i] = frame{radiusFrac: 1.0, alpha: 0.85 + 0.15*bright}     // 1.0→0.85→1.0
	}
	return fs
}

// redFrames is a calm two-beat heartbeat: a quick "lub", a softer "dub", then
// a rest, looping ~1.2 s per beat. The dot stays a solid red throughout and
// only swells gently on each beat (with a faint warm lift, never toward white),
// so it draws the eye through rhythm rather than a brightness assault — present
// and alive without reading as an alarm. It loops until you respond.
func redFrames(reduce bool) []frame {
	if reduce {
		return []frame{{radiusFrac: 1.0, alpha: 1.0}}
	}
	// bump is a smooth 0→1→0 pulse of the given width centred on center.
	bump := func(ph, center, width float64) float64 {
		d := ph - center
		if d < -width/2 || d > width/2 {
			return 0
		}
		return 0.5 + 0.5*math.Cos(2*math.Pi*d/width)
	}
	n := framesFor(1200 * time.Millisecond) // one heartbeat ≈ 1.2 s
	fs := make([]frame, n)
	for i := 0; i < n; i++ {
		ph := float64(i) / float64(n)
		// Two beats close together (lub louder than dub), then a long rest.
		beat := math.Max(bump(ph, 0.09, 0.13), 0.62*bump(ph, 0.25, 0.13))
		fs[i] = frame{
			radiusFrac: 0.86 + 0.06*beat, // a big dot; swells a little on each beat
			alpha:      1.0,
			flash:      0.30 * beat,      // a warm lift on each beat, never white
			glow:       0.55 + 1.05*beat, // steady halo, blooms bright on each beat
		}
	}
	return fs
}

// iconSize is the source bitmap size in px. macOS downscales to 16pt, so a
// larger source stays crisp on Retina; Windows wants 32; others 22.
func iconSize() int {
	switch runtime.GOOS {
	case "windows":
		return 32
	case "darwin":
		return 44
	default:
		return 22
	}
}

// motionReduced reports the macOS "Reduce Motion" accessibility setting.
// Elsewhere it is always false. Read once at startup; a change needs a
// restart, which is acceptable for an accessibility preference.
func motionReduced() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("defaults", "read", "com.apple.universalaccess", "reduceMotion").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// --- icons ---------------------------------------------------------------

// circlePNG renders a filled, edge-antialiased circle. radiusFrac scales the
// disc, alpha scales its opacity, liftFrac shifts it upward, flash lightens the
// colour toward white (0 = state colour, 1 = white), and glow adds a soft halo
// that blooms beyond the rim (0 = none). It draws into an NRGBA image so the
// colour stays true at partial alpha.
func circlePNG(c color.NRGBA, size int, radiusFrac, alpha, liftFrac, flash, glow float64) []byte {
	lerp := func(v uint8) uint8 { return uint8(float64(v) + (255-float64(v))*flash) }
	cr, cg, cb := lerp(c.R), lerp(c.G), lerp(c.B)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx := float64(size) / 2
	cy := float64(size)/2 - liftFrac*float64(size)
	r := (float64(size)/2 - 1) * radiusFrac
	gw := glow * float64(size) * 0.14 // how far the halo reaches past the rim
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			d := math.Sqrt(dx*dx + dy*dy)
			var cov float64
			switch {
			case d <= r:
				cov = 1.0
				if d > r-1 {
					cov = r - d // antialias the last pixel of the rim
				}
			case glow > 0 && d < r+gw:
				f := 1 - (d-r)/gw         // 1 at the rim → 0 at the halo's edge
				cov = glow * 0.90 * f * f // soft quadratic falloff
			default:
				continue
			}
			if cov > 1 {
				cov = 1 // a strong glow can exceed 1; clamp before it overflows
			}
			a := uint8(cov * alpha * float64(c.A))
			img.SetNRGBA(x, y, color.NRGBA{R: cr, G: cg, B: cb, A: a})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// pngToICO wraps PNG data in a minimal single-image ICO container
// (PNG-in-ICO is supported since Windows Vista).
func pngToICO(pngBytes []byte, size int) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, uint16(0))             // reserved
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))             // type: icon
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))             // image count
	buf.WriteByte(byte(size))                                         // width
	buf.WriteByte(byte(size))                                         // height
	buf.WriteByte(0)                                                  // palette colors
	buf.WriteByte(0)                                                  // reserved
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))             // planes
	_ = binary.Write(buf, binary.LittleEndian, uint16(32))            // bpp
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(pngBytes))) // data size
	_ = binary.Write(buf, binary.LittleEndian, uint32(6+16))          // data offset
	buf.Write(pngBytes)
	return buf.Bytes()
}
