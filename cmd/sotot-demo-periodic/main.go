// Command sotot-demo-periodic is a fake sotot source for testing the
// one-shot pattern: like a cron job, it publishes a snapshot of slowly
// changing obligations (renewals, taxes), waits for sotot's
// acknowledgement, and exits. The data stays fresh in sotot for
// --valid-for, then goes stale unless the job runs again.
//
// It reads the source's tailcat address (from "sotot address") from
// --addr-file or $SOTOT_ADDR.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/bradfitz/sotot/source"
)

var (
	addrFile = flag.String("addr-file", "", "file containing the source's tailcat address (default $SOTOT_ADDR)")
	validFor = flag.Duration("valid-for", 30*24*time.Hour, "how long the published data stays fresh")
	zone     = flag.String("tz", "America/Los_Angeles", "time zone of the dates")
)

func readAddr() string {
	if *addrFile != "" {
		b, err := os.ReadFile(*addrFile)
		if err != nil {
			log.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if a := os.Getenv("SOTOT_ADDR"); a != "" {
		return a
	}
	log.Fatal("need --addr-file or $SOTOT_ADDR (from: sotot address SOURCE-ID)")
	return ""
}

func main() {
	flag.Parse()
	loc, err := time.LoadLocation(*zone)
	if err != nil {
		log.Fatalf("--tz: %v", err)
	}
	now := time.Now().In(loc)
	day := func(days int) string { return now.AddDate(0, 0, days).Format(time.DateOnly) }

	things := []protocol.Thing{
		{ID: "domain:example.net", Title: "Renew example.net", Kind: "renewal", URL: "https://example.com/domains/example.net", Date: day(238)},
		{ID: "domain:sotot.dev", Title: "Renew sotot.dev (auto-renew off)", Kind: "renewal", Date: day(19)},
		{ID: "dmv:car", Title: "Renew car registration", Kind: "renewal", URL: "https://example.com/dmv", Date: day(41), Tags: []string{"car"}},
		{ID: "tax:est-q4", Title: "Pay Q4 estimated taxes", Kind: "tax", URL: "https://example.com/irs/direct-pay", Date: "2027-01-15", Tags: []string{"money"}},
		{ID: "passport", Title: "Renew passport (expires in 7 months)", Kind: "renewal", Body: "Processing takes 6-8 weeks; many countries need 6 months' validity.", Date: day(212)},
	}
	for i := range things {
		things[i].TimeZone = *zone
	}

	c := &source.Client{Addr: readAddr()}
	defer c.Close()
	c.SetValidFor(*validFor)
	if err := c.Set(things); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.PublishOnce(ctx); err != nil {
		log.Fatalf("publishing: %v", err)
	}
}
