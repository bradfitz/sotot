package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// debugCmd prints how the threads matching --labels carry their labels,
// message by message, to explain why a thread does or doesn't match. It
// prints no subjects, senders, or names of labels other than the ones
// asked about and Gmail's system labels.
func debugCmd(args []string) error {
	fs := flag.NewFlagSet("debug", flag.ExitOnError)
	token := fs.String("token", os.Getenv("SOTOT_GMAIL_TOKEN_FILE"), "token file (default $SOTOT_GMAIL_TOKEN_FILE)")
	labels := fs.String("labels", "INBOX,todo", "comma-separated labels")
	fs.Parse(args)
	ctx := context.Background()
	gm, err := newGmailFromTokenFile(ctx, *token)
	if err != nil {
		return err
	}
	names := strings.Split(*labels, ",")
	ids, err := gm.labelIDs(ctx, names)
	if err != nil {
		return err
	}
	show := map[string]string{} // label ID -> name to print
	for i, id := range ids {
		show[id] = names[i]
	}
	for _, sys := range []string{"INBOX", "SENT", "DRAFT", "UNREAD", "STARRED", "IMPORTANT", "SPAM", "TRASH", "SNOOZED"} {
		show[sys] = sys
	}
	threads, err := gm.threadsWithLabels(ctx, names, 100)
	if err != nil {
		return err
	}
	fmt.Printf("%d threads match %s\n", len(threads), *labels)
	for i, th := range threads {
		var res struct {
			Messages []struct {
				LabelIDs []string `json:"labelIds"`
			} `json:"messages"`
		}
		q := url.Values{"format": {"minimal"}, "fields": {"messages(labelIds)"}}
		if err := gm.get(ctx, "threads/"+url.PathEscape(th.ID), q, &res); err != nil {
			return err
		}
		fmt.Printf("thread %d: %d messages\n", i+1, len(res.Messages))
		for j, m := range res.Messages {
			var ls []string
			other := 0
			for _, id := range m.LabelIDs {
				if n, ok := show[id]; ok {
					ls = append(ls, n)
				} else if !strings.HasPrefix(id, "CATEGORY_") {
					other++
				}
			}
			sort.Strings(ls)
			fmt.Printf("  message %d: %s (+%d other labels)\n", j+1, strings.Join(ls, " "), other)
		}
	}
	return nil
}
