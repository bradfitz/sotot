package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGmail serves enough of the Gmail API for the watcher.
func fakeGmail(t *testing.T, threadGets *atomic.Int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/"); {
		case p == "labels":
			fmt.Fprint(w, `{"labels":[{"id":"INBOX","name":"INBOX"},{"id":"Label_7","name":"todo"},{"id":"Label_8","name":"other"}]}`)
		case p == "threads":
			ids := r.URL.Query()["labelIds"]
			if strings.Join(ids, ",") != "INBOX,Label_7" {
				t.Errorf("labelIds = %v", ids)
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"threads":[{"id":"t1","historyId":"10"}],"nextPageToken":"p2"}`)
			} else {
				fmt.Fprint(w, `{"threads":[{"id":"t2","historyId":"20"}]}`)
			}
		case strings.HasPrefix(p, "threads/"):
			threadGets.Add(1)
			if q := r.URL.Query(); q.Get("format") != "metadata" {
				t.Errorf("thread fetched with format %q; the metadata scope allows only metadata", q.Get("format"))
			}
			switch strings.TrimPrefix(p, "threads/") {
			case "t1":
				fmt.Fprint(w, `{"messages":[
					{"internalDate":"1762300000000","payload":{"headers":[{"name":"Subject","value":"Lease renewal"},{"name":"From","value":"\"Pat Landlord\" <pat@example.com>"}]}},
					{"internalDate":"1762390000000","payload":{"headers":[{"name":"Subject","value":"Re: Lease renewal"},{"name":"From","value":"bob@example.com"}]}}]}`)
			case "t2":
				fmt.Fprint(w, `{"messages":[{"internalDate":"1762380000000","payload":{"headers":[{"name":"From","value":"<noreply@example.com>"}]}}]}`)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestPoll(t *testing.T) {
	var gets atomic.Int32
	ts := fakeGmail(t, &gets)
	w := &watcher{
		gm:      &gmailClient{hc: ts.Client(), base: ts.URL},
		labels:  []string{"inbox", "TODO"}, // case-insensitive
		account: 1,
		cache:   map[string]threadInfo{},
	}
	things, sum, err := w.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum != "2 to do" || len(things) != 2 {
		t.Fatalf("summary %q, %d things", sum, len(things))
	}
	t1 := things[0]
	if t1.ID != "thread:t1" || t1.Title != "bob@example.com: Lease renewal" ||
		t1.URL != "https://mail.google.com/mail/u/1/#inbox/t1" || !t1.AttentionAt.Equal(time.UnixMilli(1762390000000)) {
		t.Errorf("t1 = %+v", t1)
	}
	if t2 := things[1]; t2.Title != "noreply@example.com: (no subject)" {
		t.Errorf("t2 = %+v", t2)
	}
	for _, th := range things {
		if err := th.Validate(); err != nil {
			t.Error(err)
		}
	}
	// Unchanged threads (same historyId) aren't fetched again.
	before := gets.Load()
	if _, _, err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != before {
		t.Errorf("refetched unchanged threads")
	}
}

func TestMissingLabel(t *testing.T) {
	var gets atomic.Int32
	ts := fakeGmail(t, &gets)
	gm := &gmailClient{hc: ts.Client(), base: ts.URL}
	_, err := gm.threadsWithLabels(context.Background(), []string{"INBOX", "nope"}, 10)
	if err == nil || !strings.Contains(err.Error(), "no Gmail label named nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		`"Jane Doe" <jane@example.com>`: "Jane Doe",
		`Jane Doe <jane@example.com>`:   "Jane Doe",
		`<jane@example.com>`:            "jane@example.com",
		`jane@example.com`:              "jane@example.com",
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestErrorThing(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	// A revoked authorization shows at once...
	if th, ok := errorThing(fmt.Errorf("%w: Token has been expired or revoked", errReauth), since, time.Hour); !ok || !strings.Contains(th.Title, "sotot-gmail login") {
		t.Errorf("reauth: %v %+v", ok, th)
	}
	// ...other errors only once they've persisted.
	if _, ok := errorThing(errors.New("503"), since, time.Hour); ok {
		t.Error("transient error shown immediately")
	}
	if _, ok := errorThing(errors.New("503"), since, time.Second); !ok {
		t.Error("persistent error not shown")
	}
}
