// Package web holds the dashboard's browser client: a Go/Wasm program
// that keeps the page live, plus its static files.
//
// The Wasm binary and its matching wasm_exec.js are generated, not
// checked in. Build them with:
//
//	go generate ./web
package web

import (
	"embed"
	"io/fs"
)

//go:generate env GOOS=js GOARCH=wasm go build -trimpath -ldflags=-s -o static/client.wasm ./client
//go:generate sh -c "install -m 0644 \"$(go env GOROOT)/lib/wasm/wasm_exec.js\" static/wasm_exec.js"

//go:embed static
var static embed.FS

// Static returns the files served under /static/.
func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
