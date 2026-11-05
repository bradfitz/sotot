package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPorkbunList(t *testing.T) {
	var starts []int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIKey string `json:"apikey"`
			Secret string `json:"secretapikey"`
			Start  int    `json:"start"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/domain/listAll" || req.APIKey != "pk1_x" || req.Secret != "sk1_y" {
			io.WriteString(w, `{"status":"ERROR","message":"bad request"}`)
			return
		}
		starts = append(starts, req.Start)
		var ds []string
		n := 1000
		if req.Start > 0 {
			n = 2
		}
		for i := 0; i < n; i++ {
			auto := `1`
			if i%2 == 1 {
				auto = `"0"` // string form, seen from older API versions
			}
			ds = append(ds, fmt.Sprintf(`{"domain":"d%d-%d.com","status":"ACTIVE","tld":"com","createDate":"2021-01-15 10:00:00","expireDate":"2027-01-15 10:00:00","autoRenew":%s}`, req.Start, i, auto))
		}
		fmt.Fprintf(w, `{"status":"SUCCESS","count":%d,"domains":[%s]}`, n, strings.Join(ds, ","))
	}))
	defer ts.Close()
	regs, err := (&porkbun{apiKey: "pk1_x", secretKey: "sk1_y", base: ts.URL}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1002 || fmt.Sprint(starts) != "[0 1000]" {
		t.Fatalf("got %d domains with starts %v", len(regs), starts)
	}
	if r := regs[0]; !r.AutoRenew || !r.Expires.Equal(time.Date(2027, 1, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first: %+v", r)
	}
	if regs[1].AutoRenew {
		t.Errorf("string \"0\" autoRenew decoded as true")
	}
}
