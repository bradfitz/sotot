package main

import (
	"bufio"
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// frame is one rendered image.
type frame struct {
	seq int
	img *image.RGBA
	png []byte
	at  time.Time
}

// hub holds recently rendered frames and wakes streams when a new one
// is published.
type hub struct {
	mu      sync.Mutex
	recent  []*frame      // oldest first, at most 5
	changed chan struct{} // closed and replaced on each publish
}

func newHub() *hub { return &hub{changed: make(chan struct{})} }

func (h *hub) publish(img *image.RGBA) *frame {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		panic(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f := &frame{img: img, png: buf.Bytes(), at: time.Now()}
	if n := len(h.recent); n > 0 {
		f.seq = h.recent[n-1].seq + 1
	} else {
		f.seq = 1
	}
	h.recent = append(h.recent, f)
	if len(h.recent) > 5 {
		h.recent = h.recent[1:]
	}
	close(h.changed)
	h.changed = make(chan struct{})
	return f
}

// latest returns the newest frame (nil if none yet) and a channel that's
// closed when a newer one is published.
func (h *hub) latest() (*frame, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.recent) == 0 {
		return nil, h.changed
	}
	return h.recent[len(h.recent)-1], h.changed
}

func (h *hub) get(seq int) *frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.recent {
		if f.seq == seq {
			return f
		}
	}
	return nil
}

func setStreamHeaders(w http.ResponseWriter, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// serveFrame serves /frame/<seq>.png, one still frame per URL so the device
// can't show a cached older one.
func (h *hub) serveFrame(w http.ResponseWriter, r *http.Request) {
	s := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/frame/"), ".png")
	seq, err := strconv.Atoi(s)
	f := h.get(seq)
	if err != nil || f == nil {
		http.NotFound(w, r)
		return
	}
	log.Printf("frame %d: %s by %s %v after render", f.seq, r.Method, r.RemoteAddr, time.Since(f.at).Round(time.Millisecond))
	setStreamHeaders(w, "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(f.png)))
	w.Write(f.png)
}

// waitNext blocks until a frame newer than seq is published or the request
// is canceled.
func (h *hub) waitNext(r *http.Request, seq int) (*frame, bool) {
	for {
		f, changed := h.latest()
		if f != nil && f.seq > seq {
			return f, true
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return nil, false
		}
	}
}

// serveMultipart serves an endless multipart/x-mixed-replace stream of
// PNGs, the "server push" technique used by MJPEG cameras. Browsers
// replace the displayed image with each part.
func (h *hub) serveMultipart(w http.ResponseWriter, r *http.Request) {
	const boundary = "sototframe"
	log.Printf("multipart: %s by %s started", r.Method, r.RemoteAddr)
	defer log.Printf("multipart: stream to %s ended", r.RemoteAddr)
	setStreamHeaders(w, "multipart/x-mixed-replace; boundary="+boundary)
	rc := http.NewResponseController(w)
	seq := 0
	for {
		f, ok := h.waitNext(r, seq)
		if !ok {
			return
		}
		seq = f.seq
		fmt.Fprintf(w, "--%s\r\nContent-Type: image/png\r\nContent-Length: %d\r\n\r\n", boundary, len(f.png))
		w.Write(f.png)
		if _, err := w.Write([]byte("\r\n")); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		log.Printf("multipart: frame %d sent to %s", f.seq, r.RemoteAddr)
	}
}

// gifPalette is a fixed 256-color palette shared by all frames: the
// background, a ramp from background to foreground for anti-aliased text,
// and a 6x6x6 color cube for everything else.
var gifPalette = func() color.Palette {
	p := color.Palette{bg}
	const ramp = 39
	for i := 1; i <= ramp; i++ {
		lerp := func(a, b uint8) uint8 { return uint8(int(a) + (int(b)-int(a))*i/ramp) }
		p = append(p, color.RGBA{lerp(bg.R, fg.R), lerp(bg.G, fg.G), lerp(bg.B, fg.B), 0xff})
	}
	for r := 0; r < 6; r++ {
		for g := 0; g < 6; g++ {
			for b := 0; b < 6; b++ {
				p = append(p, color.RGBA{uint8(r * 51), uint8(g * 51), uint8(b * 51), 0xff})
			}
		}
	}
	return p
}()

// gifQuantizer maps RGBA pixels to gifPalette indexes, caching lookups
// since frames contain few distinct colors.
type gifQuantizer struct {
	cache map[[3]uint8]uint8
}

func (q *gifQuantizer) quantize(src *image.RGBA) *image.Paletted {
	if q.cache == nil {
		q.cache = map[[3]uint8]uint8{}
	}
	b := src.Bounds()
	dst := image.NewPaletted(b, gifPalette)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		si := src.PixOffset(b.Min.X, y)
		di := dst.PixOffset(b.Min.X, y)
		for x := b.Min.X; x < b.Max.X; x++ {
			k := [3]uint8{src.Pix[si], src.Pix[si+1], src.Pix[si+2]}
			idx, ok := q.cache[k]
			if !ok {
				idx = uint8(gifPalette.Index(color.RGBA{k[0], k[1], k[2], 0xff}))
				q.cache[k] = idx
			}
			dst.Pix[di] = idx
			si += 4
			di++
		}
	}
	return dst
}

// changedRect returns the bounding box of pixels differing between a and
// b, which have the same bounds.
func changedRect(a, b *image.Paletted) image.Rectangle {
	bounds := a.Bounds()
	r := image.Rectangle{}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		ra := a.Pix[a.PixOffset(bounds.Min.X, y):a.PixOffset(bounds.Max.X, y)]
		rb := b.Pix[b.PixOffset(bounds.Min.X, y):b.PixOffset(bounds.Max.X, y)]
		if bytes.Equal(ra, rb) {
			continue
		}
		x0, x1 := 0, len(ra)
		for ra[x0] == rb[x0] {
			x0++
		}
		for ra[x1-1] == rb[x1-1] {
			x1--
		}
		r = r.Union(image.Rect(bounds.Min.X+x0, y, bounds.Min.X+x1, y+1))
	}
	return r
}

// serveGIF serves a never-ending animated GIF, writing a new frame (just
// the changed rectangle) whenever one is published.
func (h *hub) serveGIF(w http.ResponseWriter, r *http.Request) {
	log.Printf("gif: %s by %s started", r.Method, r.RemoteAddr)
	defer log.Printf("gif: stream to %s ended", r.RemoteAddr)
	setStreamHeaders(w, "image/gif")
	rc := http.NewResponseController(w)
	bw := bufio.NewWriterSize(w, 64<<10)

	var q gifQuantizer
	var prev *image.Paletted
	seq := 0
	for {
		f, ok := h.waitNext(r, seq)
		if !ok {
			return
		}
		seq = f.seq
		cur := q.quantize(f.img)
		rect := cur.Bounds()
		if prev == nil {
			writeGIFHeader(bw, rect.Dx(), rect.Dy())
		} else {
			rect = changedRect(prev, cur)
		}
		prev = cur
		if rect.Empty() {
			continue
		}
		if err := writeGIFFrame(bw, cur, rect); err != nil {
			return
		}
		if bw.Flush() != nil || rc.Flush() != nil {
			return
		}
		log.Printf("gif: frame %d (%v) sent to %s", f.seq, rect, r.RemoteAddr)
	}
}

func writeGIFHeader(w *bufio.Writer, width, height int) {
	w.WriteString("GIF89a")
	binary.Write(w, binary.LittleEndian, uint16(width))
	binary.Write(w, binary.LittleEndian, uint16(height))
	w.WriteByte(0xf7) // global color table, 8 bits/color, 256 entries
	w.WriteByte(0)    // background color index
	w.WriteByte(0)    // pixel aspect ratio
	for _, c := range gifPalette {
		rgba := c.(color.RGBA)
		w.Write([]byte{rgba.R, rgba.G, rgba.B})
	}
}

// writeGIFFrame writes the rect portion of img as a frame drawn over the
// previous ones.
func writeGIFFrame(w *bufio.Writer, img *image.Paletted, rect image.Rectangle) error {
	// Graphic control extension: disposal "do not dispose" so later
	// partial frames draw over this one; minimal delay.
	w.Write([]byte{0x21, 0xf9, 0x04, 0x04, 1, 0, 0, 0})

	w.WriteByte(0x2c) // image descriptor
	for _, v := range []int{rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy()} {
		binary.Write(w, binary.LittleEndian, uint16(v))
	}
	w.WriteByte(0) // no local color table, not interlaced

	w.WriteByte(8) // LZW minimum code size
	bw := &gifBlockWriter{w: w}
	lw := lzw.NewWriter(bw, lzw.LSB, 8)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		lw.Write(img.Pix[img.PixOffset(rect.Min.X, y):img.PixOffset(rect.Max.X, y)])
	}
	if err := lw.Close(); err != nil {
		return err
	}
	bw.flush()
	return w.WriteByte(0) // block terminator
}

// gifBlockWriter splits LZW output into GIF data sub-blocks of at most
// 255 bytes, each prefixed by its length.
type gifBlockWriter struct {
	w   *bufio.Writer
	buf [255]byte
	n   int
}

func (b *gifBlockWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		c := copy(b.buf[b.n:], p)
		b.n += c
		p = p[c:]
		if b.n == len(b.buf) {
			b.flush()
		}
	}
	return total, nil
}

func (b *gifBlockWriter) flush() {
	if b.n == 0 {
		return
	}
	b.w.WriteByte(byte(b.n))
	b.w.Write(b.buf[:b.n])
	b.n = 0
}
