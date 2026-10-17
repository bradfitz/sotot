package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// renderer draws a test pattern for judging how faithfully the device
// displays pixels: a clock to see update latency, text at several sizes
// and colors, and 1px line patterns that blur visibly if the image is
// rescaled or compressed.
type renderer struct {
	w, h  int
	label string // shown on screen, e.g. the casting mode
	font  *opentype.Font
	faces map[float64]font.Face
}

func newRenderer(w, h int, label string) *renderer {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		panic(err)
	}
	return &renderer{w: w, h: h, label: label, font: f, faces: map[float64]font.Face{}}
}

func (r *renderer) face(size float64) font.Face {
	if f, ok := r.faces[size]; ok {
		return f
	}
	f, err := opentype.NewFace(r.font, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	r.faces[size] = f
	return f
}

var (
	bg = color.RGBA{0x10, 0x14, 0x1c, 0xff}
	fg = color.RGBA{0xe8, 0xea, 0xee, 0xff}

	testColors = []struct {
		name string
		c    color.RGBA
	}{
		{"red", color.RGBA{0xf0, 0x50, 0x50, 0xff}},
		{"green", color.RGBA{0x40, 0xd0, 0x60, 0xff}},
		{"dark green", color.RGBA{0x20, 0x80, 0x40, 0xff}},
		{"blue", color.RGBA{0x50, 0x90, 0xf0, 0xff}},
		{"yellow", color.RGBA{0xf0, 0xd0, 0x40, 0xff}},
	}
)

// text draws s with its baseline at (x, y) and returns the x just past it.
func (r *renderer) text(img *image.RGBA, x, y int, size float64, c color.Color, s string) int {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: r.face(size),
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
	return d.Dot.X.Round()
}

func (r *renderer) render(now time.Time) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, r.w, r.h))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)

	m := r.w / 30 // margin
	r.text(img, m, m+60, 64, fg, "Hello from sotot-cast")
	r.text(img, m, m+200, 120, fg, now.Format("15:04:05"))
	r.text(img, m, m+250, 24, fg, fmt.Sprintf("mode %s, %dx%d, rendered %s", r.label, r.w, r.h, now.Format(time.RFC3339)))

	y := m + 320
	for _, size := range []float64{10, 12, 14, 16, 20, 24, 32} {
		r.text(img, m, y, size, fg, fmt.Sprintf("%gpx: The quick brown fox jumps over the lazy dog 0123456789 Il1|", size))
		y += int(size*1.5) + 8
	}
	for _, size := range []float64{14, 20} {
		y += 8
		x := m
		for _, tc := range testColors {
			x = r.text(img, x, y, size, tc.c, fmt.Sprintf("%gpx %s", size, strings.ToUpper(tc.name))) + int(size)
		}
		y += int(size * 1.5)
	}

	// Line patterns at the right: alternating 1px, 2px, and 3px stripes,
	// vertical then horizontal. If the device scales or compresses the
	// image, the 1px stripes turn into gray mush or moiré.
	px := r.w * 2 / 3
	py := m + 320
	for i, period := range []int{1, 2, 3} {
		x0 := px + i*(r.w/10)
		x1 := x0 + r.w/12
		for x := x0; x < x1; x++ {
			for y := py; y < py+r.h/5; y++ {
				if ((x-x0)/period)%2 == 0 {
					img.SetRGBA(x, y, fg)
				}
			}
		}
		for x := x0; x < x1; x++ {
			for y := py + r.h/5 + 20; y < py+2*r.h/5+20; y++ {
				if ((y-py)/period)%2 == 0 {
					img.SetRGBA(x, y, fg)
				}
			}
		}
		r.text(img, x0, py+2*r.h/5+50, 20, fg, fmt.Sprintf("%dpx", period))
	}

	// 1px border to check for overscan cropping.
	for x := 0; x < r.w; x++ {
		img.SetRGBA(x, 0, fg)
		img.SetRGBA(x, r.h-1, fg)
	}
	for y := 0; y < r.h; y++ {
		img.SetRGBA(0, y, fg)
		img.SetRGBA(r.w-1, y, fg)
	}
	return img
}
