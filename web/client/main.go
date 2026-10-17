//go:build js && wasm

// Command client is the dashboard's browser client. It keeps the page
// live: it receives freshly rendered views over a WebSocket and swaps
// them in, and warns when updates stop, so a stale page never looks
// live.
package main

import (
	"syscall/js"
	"time"
)

// silenceLimit is how long without a message (the server sends at least
// every 30s) before the connection is presumed dead.
const silenceLimit = 75 * time.Second

func main() {
	doc := js.Global().Get("document")
	view := doc.Call("getElementById", "view")
	conn := doc.Call("getElementById", "conn")
	loc := js.Global().Get("location")
	scheme := "ws:"
	if loc.Get("protocol").String() == "https:" {
		scheme = "wss:"
	}
	url := scheme + "//" + loc.Get("host").String() + "/ws"

	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	for {
		if run(url, view, conn) {
			backoff = minBackoff
		}
		conn.Set("textContent", "Live updates unavailable; reconnecting. Statuses below may be out of date.")
		conn.Set("hidden", false)
		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

// run connects once and applies views until the connection ends. It
// reports whether any view arrived.
func run(url string, view, conn js.Value) (gotView bool) {
	ws := js.Global().Get("WebSocket").New(url)
	msgs := make(chan string, 16)
	closed := make(chan struct{})
	var funcs []js.Func
	on := func(event string, f func(js.Value)) {
		fn := js.FuncOf(func(this js.Value, args []js.Value) any {
			f(args[0])
			return nil
		})
		funcs = append(funcs, fn)
		ws.Set(event, fn)
	}
	on("onmessage", func(ev js.Value) {
		select {
		case msgs <- ev.Get("data").String():
		default: // a newer view will follow
		}
	})
	closeOnce := false
	on("onclose", func(js.Value) {
		if !closeOnce {
			closeOnce = true
			close(closed)
		}
	})
	defer func() {
		for _, fn := range funcs {
			fn.Release()
		}
	}()

	timer := time.NewTimer(silenceLimit)
	defer timer.Stop()
	for {
		select {
		case <-closed:
			return gotView
		case <-timer.C:
			ws.Call("close")
			<-closed
			return gotView
		case data := <-msgs:
			// Parse with the browser's JSON rather than encoding/json,
			// which would triple the size of this binary.
			m := parseJSON(data)
			if m.Type() != js.TypeObject || m.Get("type").String() != "view" {
				continue
			}
			view.Set("innerHTML", m.Get("html")) // rendered and escaped by the server
			conn.Set("hidden", true)
			gotView = true
			timer.Reset(silenceLimit)
		}
	}
}

// parseJSON returns data parsed by JSON.parse, or undefined if invalid.
func parseJSON(data string) (v js.Value) {
	defer func() {
		if recover() != nil {
			v = js.Undefined()
		}
	}()
	return js.Global().Get("JSON").Call("parse", data)
}
