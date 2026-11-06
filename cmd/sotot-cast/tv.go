package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"time"

	"github.com/bradfitz/sotot/internal/present"
	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// tvRenderer draws the dashboard for a TV across the room: large type
// (nothing under about 24px at 1080p), few words, and status shown with
// labels as well as color.
type tvRenderer struct {
	w, h  int
	scale float64 // relative to 1080p
	loc   *time.Location
	fonts map[bool]*opentype.Font // keyed by bold
	faces map[faceKey]font.Face
}

type faceKey struct {
	size float64
	bold bool
}

var (
	tvMuted     = color.RGBA{0x9a, 0xa3, 0xb2, 0xff}
	tvPanel     = color.RGBA{0x18, 0x1e, 0x29, 0xff}
	tvBorder    = color.RGBA{0x2c, 0x36, 0x48, 0xff}
	tvGreen     = color.RGBA{0x3d, 0xdc, 0x6c, 0xff}
	tvDarkGreen = color.RGBA{0x1f, 0x7a, 0x3c, 0xff}
	tvRed       = color.RGBA{0xf0, 0x50, 0x50, 0xff}
	tvRedBg     = color.RGBA{0x4a, 0x1a, 0x1e, 0xff}
)

func newTVRenderer(w, h int, loc *time.Location) *tvRenderer {
	r := &tvRenderer{w: w, h: h, scale: float64(h) / 1080, loc: loc, fonts: map[bool]*opentype.Font{}, faces: map[faceKey]font.Face{}}
	for bold, ttf := range map[bool][]byte{false: goregular.TTF, true: gobold.TTF} {
		f, err := opentype.Parse(ttf)
		if err != nil {
			panic(err)
		}
		r.fonts[bold] = f
	}
	return r
}

// face returns a face of size px at 1080p, scaled to the frame.
func (r *tvRenderer) face(px float64, bold bool) font.Face {
	k := faceKey{px * r.scale, bold}
	if f, ok := r.faces[k]; ok {
		return f
	}
	f, err := opentype.NewFace(r.fonts[bold], &opentype.FaceOptions{Size: k.size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	r.faces[k] = f
	return f
}

func (r *tvRenderer) px(v float64) int { return int(math.Round(v * r.scale)) }

// text draws s with its baseline at (x, y), truncated with an ellipsis to
// fit maxW if maxW > 0, and returns the x just past it.
func (r *tvRenderer) text(img *image.RGBA, x, y int, px float64, bold bool, c color.Color, s string, maxW int) int {
	f := r.face(px, bold)
	if maxW > 0 {
		s = fitText(f, s, maxW)
	}
	d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
	return d.Dot.X.Round()
}

func textWidth(f font.Face, s string) int { return font.MeasureString(f, s).Round() }

// fitText truncates s with "…" so it's at most maxW pixels wide.
func fitText(f font.Face, s string, maxW int) string {
	if textWidth(f, s) <= maxW {
		return s
	}
	rs := []rune(s)
	lo, hi := 0, len(rs)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if textWidth(f, strings.TrimRight(string(rs[:mid]), " ")+"…") <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(rs[:lo]), " ") + "…"
}

// roundRect fills an antialiased rounded rectangle.
func roundRect(img *image.RGBA, rect image.Rectangle, radius float64, c color.Color) {
	z := vector.NewRasterizer(rect.Dx(), rect.Dy())
	w, h := float32(rect.Dx()), float32(rect.Dy())
	rad := float32(min(radius, float64(min(w, h))/2))
	const k = 0.5523 // cubic Bézier circle approximation
	z.MoveTo(rad, 0)
	z.LineTo(w-rad, 0)
	z.CubeTo(w-rad+rad*k, 0, w, rad-rad*k, w, rad)
	z.LineTo(w, h-rad)
	z.CubeTo(w, h-rad+rad*k, w-rad+rad*k, h, w-rad, h)
	z.LineTo(rad, h)
	z.CubeTo(rad-rad*k, h, 0, h-rad+rad*k, 0, h-rad)
	z.LineTo(0, rad)
	z.CubeTo(0, rad-rad*k, rad-rad*k, 0, rad, 0)
	z.ClosePath()
	z.DrawOp = draw.Over
	z.Draw(img, rect, image.NewUniform(c), image.Point{})
}

func statusColor(s state.Status) color.Color {
	switch s {
	case state.Connected:
		return tvGreen
	case state.Fresh:
		return tvDarkGreen
	default:
		return tvRed
	}
}

func (r *tvRenderer) render(now time.Time, fs feedState) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, r.w, r.h))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	m := r.px(56) // side margin
	right := r.w - m
	n := now.In(r.loc)

	// Header: name at left, clock and date at right.
	r.text(img, m, r.px(92), 44, true, fg, "sotot", 0)
	clock := n.Format("3:04")
	cf := r.face(96, true)
	date := n.Format("Monday, January 2")
	df := r.face(32, false)
	r.text(img, right-textWidth(cf, clock), r.px(112), 96, true, fg, clock, 0)
	r.text(img, right-textWidth(df, date), r.px(156), 32, false, tvMuted, date, 0)
	y := r.px(196)

	// Connection to sotot: never let a frozen view look live.
	if !fs.Connected {
		msg := "Not connected to sotot"
		if fs.HaveView {
			msg = fmt.Sprintf("Lost connection to sotot %s; showing last known state", present.Ago(fs.Since, now))
		}
		bh := r.px(64)
		roundRect(img, image.Rect(m, y, right, y+bh), float64(r.px(10)), tvRedBg)
		r.text(img, m+r.px(24), y+r.px(43), 32, true, tvRed, msg, right-m-r.px(48))
		y += bh + r.px(24)
	}
	if !fs.HaveView {
		return img
	}
	v := fs.View

	// Urgent things get pulsing banners above everything else.
	var urgent, entries []present.Entry
	for _, e := range present.Entries(v, now, r.loc) {
		if e.Thing.Priority == protocol.PriorityUrgent {
			urgent = append(urgent, e)
		} else {
			entries = append(entries, e)
		}
	}
	y = r.drawUrgent(img, urgent, m, right, y, now)

	// Sources: a chip per source with a colored dot and a word label.
	chipH := r.px(56)
	x := m
	for _, sv := range v.Sources {
		name := sv.Name
		label := present.StatusLabel(sv)
		nf, lf := r.face(30, true), r.face(26, false)
		cw := r.px(24) + r.px(18) + r.px(14) + textWidth(nf, name) + r.px(14) + textWidth(lf, label) + r.px(28)
		if x+cw > right && x > m {
			x = m
			y += chipH + r.px(14)
		}
		roundRect(img, image.Rect(x, y, x+cw, y+chipH), float64(chipH)/2, tvPanel)
		cx, cy := x+r.px(24)+r.px(9), y+chipH/2
		dot := r.px(18)
		roundRect(img, image.Rect(cx-dot/2, cy-dot/2, cx-dot/2+dot, cy-dot/2+dot), float64(dot)/2, statusColor(sv.Status))
		tx := r.text(img, x+r.px(24)+r.px(18)+r.px(14), y+r.px(38), 30, true, fg, name, 0)
		lc := tvMuted
		if sv.Status == state.Stale || sv.Status == state.Never {
			lc = tvRed
		}
		r.text(img, tx+r.px(14), y+r.px(38), 26, false, lc, label, 0)
		x += cw + r.px(16)
	}
	y += chipH + r.px(36)

	// Things, as many as fit.
	rowH := r.px(112)
	gap := r.px(14)
	moreH := r.px(52)
	avail := r.h - r.px(40) - y
	fit := avail / (rowH + gap)
	if len(entries) > fit {
		fit = (avail - moreH) / (rowH + gap)
	}
	for i, e := range entries {
		if i >= fit {
			r.text(img, m, y+r.px(36), 32, false, tvMuted, fmt.Sprintf("+ %d more", len(entries)-i), 0)
			break
		}
		r.drawEntry(img, image.Rect(m, y, right, y+rowH), e)
		y += rowH + gap
	}
	if len(entries) == 0 {
		msg, c := "Nothing needs attention.", tvMuted
		for _, sv := range v.Sources {
			if sv.Status == state.Stale || sv.Status == state.Never {
				msg, c = "Nothing reported, but some sources are stale or have never reported.", tvRed
			}
		}
		r.text(img, m, y+r.px(48), 40, false, c, msg, right-m)
	}
	return img
}

// pulsePeriod is how long the urgent banner's pulse takes.
const pulsePeriod = 1600 * time.Millisecond

// animating reports whether the view has moving parts (urgent banners),
// so needs frames more often than once a second.
func (r *tvRenderer) animating(fs feedState) bool {
	if !fs.HaveView {
		return false
	}
	for _, sv := range fs.View.Sources {
		for _, t := range sv.Things {
			if t.Priority == protocol.PriorityUrgent && !t.Upcoming(time.Now()) {
				return true
			}
		}
	}
	return false
}

// drawUrgent draws a pulsing full-width banner for each of up to two
// urgent entries, starting at y, and returns the y below them.
func (r *tvRenderer) drawUrgent(img *image.RGBA, es []present.Entry, left, right, y int, now time.Time) int {
	if len(es) == 0 {
		return y
	}
	// Pulse between dark and bright red.
	phase := float64(now.UnixMilli()%pulsePeriod.Milliseconds()) / float64(pulsePeriod.Milliseconds())
	k := (1 - math.Cos(2*math.Pi*phase)) / 2 // 0..1..0
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*k) }
	dark, bright := color.RGBA{0x6a, 0x10, 0x14, 0xff}, color.RGBA{0xe8, 0x30, 0x30, 0xff}
	bg := color.RGBA{lerp(dark.R, bright.R), lerp(dark.G, bright.G), lerp(dark.B, bright.B), 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}

	const maxBanners = 2
	bh := r.px(150)
	for i, e := range es {
		if i == maxBanners {
			r.text(img, left, y+r.px(36), 32, true, tvRed, fmt.Sprintf("+ %d more urgent", len(es)-i), 0)
			y += r.px(52)
			break
		}
		roundRect(img, image.Rect(left, y, right, y+bh), float64(r.px(18)), bg)
		x := left + r.px(36)
		maxX := right - r.px(36)
		r.text(img, x, y+r.px(72), 58, true, white, e.Thing.Title, maxX-x)
		meta := e.SourceName
		if w := e.When; w != nil {
			meta = w.Relative + " · " + meta
		}
		r.text(img, x, y+r.px(124), 34, true, white, meta, maxX-x)
		y += bh + r.px(18)
	}
	return y + r.px(10)
}

func (r *tvRenderer) drawEntry(img *image.RGBA, rect image.Rectangle, e present.Entry) {
	border := tvPanel
	if e.SourceStatus == state.Stale {
		border = tvRed
	}
	roundRect(img, rect, float64(r.px(14)), border)
	inset := r.px(3)
	roundRect(img, rect.Inset(inset), float64(r.px(12)), tvPanel)
	high := e.Thing.Priority == protocol.PriorityHigh
	titleColor := color.Color(fg)
	switch e.Thing.Priority {
	case protocol.PriorityHigh:
		// A red bar down the left edge.
		bar := image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+r.px(12), rect.Max.Y)
		roundRect(img, bar, float64(r.px(6)), tvRed)
	case protocol.PriorityLow:
		titleColor = tvMuted
	}

	x := rect.Min.X + r.px(28)
	maxX := rect.Max.X - r.px(28)
	// Title line, with the kind as a small label in front.
	if k := kindLabel(e.Thing.Kind); k != "" {
		kf := r.face(24, true)
		kw := textWidth(kf, k) + r.px(24)
		ky := rect.Min.Y + r.px(18)
		roundRect(img, image.Rect(x, ky, x+kw, ky+r.px(36)), float64(r.px(8)), tvBorder)
		r.text(img, x+r.px(12), ky+r.px(27), 24, true, tvMuted, k, 0)
		x += kw + r.px(16)
	}
	r.text(img, x, rect.Min.Y+r.px(48), 38, high, titleColor, e.Thing.Title, maxX-x)

	// Meta line: when, and from which source.
	x = rect.Min.X + r.px(28)
	y := rect.Min.Y + r.px(92)
	if w := e.When; w != nil {
		c := tvMuted
		if high || (w.Past && e.Thing.DueAt != nil) {
			c = tvRed // pressing, or past a deadline
		}
		x = r.text(img, x, y, 28, true, c, w.Relative, 0)
		x = r.text(img, x+r.px(14), y, 28, false, tvMuted, "· "+w.Label, 0)
		x += r.px(14)
		x = r.text(img, x, y, 28, false, tvMuted, "·", 0) + r.px(14)
	}
	src := e.SourceName
	if e.SourceStatus == state.Stale {
		src += " (stale)"
	}
	r.text(img, x, y, 28, false, tvMuted, src, maxX-x)
}

// kindLabel is the short label shown before a thing's title.
func kindLabel(kind string) string {
	switch kind {
	case "":
		return ""
	case "pull_request":
		return "PR"
	default:
		return strings.ToUpper(strings.ReplaceAll(kind, "_", " "))
	}
}
