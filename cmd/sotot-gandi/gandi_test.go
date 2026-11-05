package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGandiList(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Total-Count", "3")
		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, `[
				{"fqdn":"Example.NET","dates":{"registry_created_at":"2019-02-13T10:04:18Z","registry_ends_at":"2027-02-13T10:04:18Z"},"autorenew":false,"status":["clientTransferProhibited"]},
				{"fqdn":"held.org","dates":{"registry_ends_at":"2027-05-01T00:00:00Z"},"autorenew":true,"status":["clientHold"]}]`)
		case "2":
			io.WriteString(w, `[{"fqdn":"nodate.io","dates":{},"autorenew":true,"status":[]}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer ts.Close()
	regs, err := (&gandi{token: "pat_x", base: ts.URL}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer pat_x" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(regs) != 3 {
		t.Fatalf("got %d domains: %+v", len(regs), regs)
	}
	if r := regs[0]; r.Domain != "example.net" || r.AutoRenew || !r.Expires.Equal(time.Date(2027, 2, 13, 10, 4, 18, 0, time.UTC)) || r.Problem != "" {
		t.Errorf("example.net: %+v", r)
	}
	if r := regs[1]; !r.AutoRenew || r.Problem != "clientHold" {
		t.Errorf("held.org: %+v", r)
	}
	if r := regs[2]; !r.Expires.IsZero() {
		t.Errorf("nodate.io: %+v", r)
	}
}
