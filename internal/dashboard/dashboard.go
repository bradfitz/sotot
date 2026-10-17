// Package dashboard serves sotot's read-only dashboard: a Go-rendered
// HTML page kept live over a WebSocket, and a JSON stream of the
// dashboard state for other displays such as sotot-cast.
//
// Both streams push the full current state on connection and after each
// change. Notifications coalesce, so a slow client just skips to the
// latest state and can never block ingestion.
package dashboard

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/sotot/internal/present"
	"github.com/bradfitz/sotot/internal/state"
	"github.com/coder/websocket"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server is the dashboard HTTP handler.
type Server struct {
	Store *state.Store
	// Static holds the browser client's files (client.wasm, wasm_exec.js,
	// style.css), served under /static/.
	Static fs.FS
	// Location is the time zone for displaying times; nil means local.
	Location *time.Location
	Logf     func(format string, args ...any)

	mux      *http.ServeMux
	tmpl     *template.Template
	haveWasm bool
	wasmGz   []byte // gzipped client.wasm
}

// heartbeat is how often streams resend the state even without changes,
// refreshing relative times and proving the connection is alive.
const heartbeat = 30 * time.Second

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Init prepares the server. It must be called before ServeHTTP.
func (s *Server) Init() error {
	t, err := template.New("").Funcs(template.FuncMap{
		"statusLabel":  present.StatusLabel,
		"statusDetail": present.StatusDetail,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	s.tmpl = t
	if s.Location == nil {
		s.Location = time.Local
	}
	_, err1 := fs.Stat(s.Static, "client.wasm")
	_, err2 := fs.Stat(s.Static, "wasm_exec.js")
	s.haveWasm = err1 == nil && err2 == nil
	if !s.haveWasm {
		s.logf("dashboard: browser client not built (run: go generate ./web); pages won't update live")
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /{$}", s.serveIndex)
	s.mux.HandleFunc("GET /ws", s.serveViewStream)
	s.mux.HandleFunc("GET /api/v1/view", s.serveJSON)
	s.mux.HandleFunc("GET /api/v1/stream", s.serveJSONStream)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(s.Static)))
	if s.haveWasm {
		// The Wasm client is about 2 MB, a third of that gzipped.
		b, err := fs.ReadFile(s.Static, "client.wasm")
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(b)
		zw.Close()
		s.wasmGz = buf.Bytes()
		s.mux.HandleFunc("GET /static/client.wasm", s.serveWasm)
	}
	return nil
}

func (s *Server) serveWasm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/wasm")
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		http.ServeFileFS(w, r, s.Static, "client.wasm")
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.wasmGz)))
	w.Write(s.wasmGz)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

// viewData is the template data for one rendering of the view.
type viewData struct {
	View    state.View
	Now     time.Time
	Entries []present.Entry
	// Uncertain is whether some source's data isn't current, so an
	// empty list doesn't mean nothing needs attention.
	Uncertain bool
}

func (s *Server) viewData(v state.View) viewData {
	now := time.Now()
	d := viewData{View: v, Now: now, Entries: present.Entries(v, now, s.Location)}
	for _, sv := range v.Sources {
		if sv.Status == state.Stale || sv.Status == state.Never {
			d.Uncertain = true
		}
	}
	return d
}

func (s *Server) renderView(v state.View) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "view", s.viewData(v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	err := s.tmpl.ExecuteTemplate(&buf, "index.html", map[string]any{
		"HaveWasm": s.haveWasm,
		"Data":     s.viewData(s.Store.View()),
	})
	if err != nil {
		s.logf("dashboard: rendering page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self'; connect-src 'self'")
	w.Write(buf.Bytes())
}

func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(s.Store.View())
}

// viewMessage is sent on the browser stream: the rendered view HTML.
type viewMessage struct {
	Type     string `json:"type"` // "view"
	Instance string `json:"instance"`
	Revision uint64 `json:"revision"`
	HTML     string `json:"html"`
}

// streamMessage is sent on the JSON stream: the dashboard state.
type streamMessage struct {
	Type string `json:"type"` // "view"
	state.View
}

func (s *Server) serveViewStream(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, func(v state.View) (any, error) {
		html, err := s.renderView(v)
		if err != nil {
			return nil, err
		}
		return viewMessage{Type: "view", Instance: v.Instance, Revision: v.Revision, HTML: string(html)}, nil
	})
}

func (s *Server) serveJSONStream(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, func(v state.View) (any, error) {
		return streamMessage{Type: "view", View: v}, nil
	})
}

// stream sends encode(view) on a WebSocket at connection, after each
// change, and every heartbeat.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, encode func(state.View) (any, error)) {
	// Accept verifies that browser requests are same-origin, so the
	// stream inherits the dashboard's access control.
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // we only write; this handles pings and close

	sub, v := s.Store.Subscribe()
	defer sub.Close()
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		msg, err := encode(v)
		if err != nil {
			s.logf("dashboard: %v", err)
			c.Close(websocket.StatusInternalError, "")
			return
		}
		b, err := json.Marshal(msg)
		if err != nil {
			s.logf("dashboard: %v", err)
			return
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = c.Write(wctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			// Including a client too slow to take a message within
			// the timeout: it reconnects and resynchronizes.
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-sub.C():
		case <-t.C:
		}
		v = s.Store.View()
	}
}
