package protocol

import (
	"strings"
	"testing"
)

func TestParseSourceMessage(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string // substring; empty means valid
	}{
		{"snapshot", `{"type":"snapshot","seq":1,"valid_for_seconds":2592000,"things":[{"id":"example.net","title":"Renew example.net","kind":"renewal","date":"2027-06-12","time_zone":"America/Los_Angeles"}]}`, ""},
		{"empty snapshot", `{"type":"snapshot","seq":1,"valid_for_seconds":0,"things":[]}`, ""},
		{"unknown fields ignored", `{"type":"snapshot","seq":1,"valid_for_seconds":0,"things":[{"id":"a","title":"A","future_field":1}],"extra":true}`, ""},
		{"snapshot missing validity", `{"type":"snapshot","seq":1,"things":[]}`, "missing valid_for_seconds"},
		{"negative validity", `{"type":"snapshot","seq":1,"valid_for_seconds":-1,"things":[]}`, "out of range"},
		{"zero seq", `{"type":"snapshot","seq":0,"valid_for_seconds":1,"things":[]}`, "invalid seq"},
		{"duplicate ids", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A"},{"id":"a","title":"B"}]}`, "duplicate"},
		{"missing title", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a"}]}`, "missing title"},
		{"bad url scheme", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","url":"javascript:alert(1)"}]}`, "not an absolute http"},
		{"date without zone", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","date":"2027-01-02"}]}`, "date without time_zone"},
		{"bad zone", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","date":"2027-01-02","time_zone":"Mars/Olympus"}]}`, "unknown time_zone"},
		{"date and due_at", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","date":"2027-01-02","time_zone":"UTC","due_at":"2027-01-02T00:00:00Z"}]}`, "exclusive"},
		{"timestamp without offset", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","due_at":"2027-01-02T00:00:00"}]}`, "invalid snapshot"},
		{"ends before starts", `{"type":"snapshot","seq":1,"valid_for_seconds":1,"things":[{"id":"a","title":"A","starts_at":"2027-01-02T10:00:00Z","ends_at":"2027-01-02T09:00:00Z"}]}`, "ends_at before"},
		{"delta", `{"type":"delta","seq":2,"upsert":[{"id":"pr:123","title":"Review","kind":"pull_request"}],"remove":["pr:122"]}`, ""},
		{"delta same id in both", `{"type":"delta","seq":2,"upsert":[{"id":"x","title":"X"}],"remove":["x"]}`, "more than once"},
		{"refresh", `{"type":"refresh","seq":3,"valid_for_seconds":86400}`, ""},
		{"refresh missing validity", `{"type":"refresh","seq":3}`, "missing valid_for_seconds"},
		{"unknown type", `{"type":"dismiss","seq":3}`, "unknown message type"},
		{"no type", `{"seq":3}`, "no type"},
		{"not json", `{`, "invalid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSourceMessage([]byte(tt.in))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tt.wantErr)
			}
		})
	}
}
