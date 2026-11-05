package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// scope lets sotot-gmail see labels and message headers (subject,
// sender, date), but never message bodies, and change nothing.
const scope = "https://www.googleapis.com/auth/gmail.metadata"

// errReauth means Google no longer honors the saved authorization.
var errReauth = errors.New("gmail authorization expired or was revoked")

// tokenFile is the JSON written by `sotot-gmail login`: everything the
// source needs to read Gmail. It's a secret.
type tokenFileData struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email,omitempty"` // for reference
}

func (t *tokenFileData) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     t.ClientID,
		ClientSecret: t.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{scope},
	}
}

// gmailClient calls the Gmail REST API.
type gmailClient struct {
	hc   *http.Client
	base string // overridable for tests

	mu     sync.Mutex
	labels map[string]string // lowercase name -> label ID
}

func newGmailFromTokenFile(ctx context.Context, path string) (*gmailClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t tokenFileData
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.ClientID == "" || t.RefreshToken == "" {
		return nil, fmt.Errorf("%s: missing client_id or refresh_token; rerun sotot-gmail login", path)
	}
	ts := t.config().TokenSource(ctx, &oauth2.Token{RefreshToken: t.RefreshToken})
	return &gmailClient{hc: oauth2.NewClient(ctx, ts), base: "https://gmail.googleapis.com"}, nil
}

func (g *gmailClient) get(ctx context.Context, path string, q url.Values, v any) error {
	u := g.base + "/gmail/v1/users/me/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	res, err := g.hc.Do(req)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			return fmt.Errorf("%w: %v", errReauth, re.ErrorDescription)
		}
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return fmt.Errorf("gmail: %s: %s", res.Status, msg)
	}
	return json.Unmarshal(b, v)
}

// labelIDs returns the IDs of the named labels (system ones like INBOX,
// or your own), case-insensitively.
func (g *gmailClient) labelIDs(ctx context.Context, names []string) ([]string, error) {
	lookup := func() ([]string, bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var ids []string
		for _, n := range names {
			id, ok := g.labels[strings.ToLower(n)]
			if !ok {
				return nil, false
			}
			ids = append(ids, id)
		}
		return ids, true
	}
	if ids, ok := lookup(); ok {
		return ids, nil
	}
	var res struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := g.get(ctx, "labels", nil, &res); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range res.Labels {
		m[strings.ToLower(l.Name)] = l.ID
	}
	g.mu.Lock()
	g.labels = m
	g.mu.Unlock()
	if ids, ok := lookup(); ok {
		return ids, nil
	}
	var missing []string
	for _, n := range names {
		if _, ok := m[strings.ToLower(n)]; !ok {
			missing = append(missing, n)
		}
	}
	return nil, fmt.Errorf("no Gmail label named %s", strings.Join(missing, ", "))
}

type threadRef struct {
	ID        string `json:"id"`
	HistoryID string `json:"historyId"`
}

// threadsWithLabels returns up to max threads having all the named labels.
func (g *gmailClient) threadsWithLabels(ctx context.Context, labels []string, max int) ([]threadRef, error) {
	ids, err := g.labelIDs(ctx, labels)
	if err != nil {
		return nil, err
	}
	var all []threadRef
	page := ""
	for len(all) < max {
		q := url.Values{"maxResults": {"100"}}
		for _, id := range ids {
			q.Add("labelIds", id)
		}
		if page != "" {
			q.Set("pageToken", page)
		}
		var res struct {
			Threads       []threadRef `json:"threads"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := g.get(ctx, "threads", q, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Threads...)
		if res.NextPageToken == "" {
			break
		}
		page = res.NextPageToken
	}
	if len(all) > max {
		all = all[:max]
	}
	return all, nil
}

// threadInfo returns a thread's subject (from its first message), and
// the sender and date of its newest message.
func (g *gmailClient) threadInfo(ctx context.Context, id string) (threadInfo, error) {
	q := url.Values{
		"format":          {"metadata"},
		"metadataHeaders": {"Subject", "From"},
		"fields":          {"messages(internalDate,payload/headers)"},
	}
	var res struct {
		Messages []struct {
			InternalDate string `json:"internalDate"` // ms since epoch
			Payload      struct {
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"payload"`
		} `json:"messages"`
	}
	if err := g.get(ctx, "threads/"+url.PathEscape(id), q, &res); err != nil {
		return threadInfo{}, err
	}
	var info threadInfo
	for i, m := range res.Messages {
		for _, h := range m.Payload.Headers {
			switch {
			case strings.EqualFold(h.Name, "Subject") && i == 0:
				info.subject = h.Value
			case strings.EqualFold(h.Name, "From"):
				info.from = displayName(h.Value)
			}
		}
		if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil {
			if t := time.UnixMilli(ms); t.After(info.last) {
				info.last = t
			}
		}
	}
	return info, nil
}

// displayName returns the name part of an address like
// `"Jane Doe" <jane@example.com>`, or the address if there's no name.
func displayName(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.LastIndex(from, "<"); i > 0 {
		if name := strings.Trim(strings.TrimSpace(from[:i]), `"`); name != "" {
			return name
		}
	}
	return strings.Trim(from, "<>")
}

// historyID returns the ID of the mailbox's current history record,
// which increases whenever anything in the mailbox changes.
func (g *gmailClient) historyID(ctx context.Context) (string, error) {
	var res struct {
		HistoryID string `json:"historyId"`
	}
	if err := g.get(ctx, "profile", url.Values{"fields": {"historyId"}}, &res); err != nil {
		return "", err
	}
	return res.HistoryID, nil
}

func (g *gmailClient) emailAddress(ctx context.Context) (string, error) {
	var res struct {
		EmailAddress string `json:"emailAddress"`
	}
	err := g.get(ctx, "profile", nil, &res)
	return res.EmailAddress, err
}

// loginCmd runs the one-time interactive authorization and writes the
// token file.
func loginCmd(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	clientFile := fs.String("client", "", "OAuth client JSON downloaded from Google Cloud (\"Desktop app\" type)")
	out := fs.String("token", "", "token file to write; a secret")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sotot-gmail login --client=client_secret_....json --token=gmail-token.json")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *clientFile == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}
	b, err := os.ReadFile(*clientFile)
	if err != nil {
		return err
	}
	cfg, err := google.ConfigFromJSON(b, scope)
	if err != nil {
		return fmt.Errorf("%s: %w (want the JSON of a \"Desktop app\" OAuth client)", *clientFile, err)
	}

	// The loopback redirect flow, with PKCE.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	verifier := oauth2.GenerateVerifier()
	var sb [16]byte
	rand.Read(sb[:])
	state := hex.EncodeToString(sb[:])
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"), // always issue a refresh token
	)

	codes := make(chan string, 1)
	errc := make(chan error, 1)
	accept := func(rawURL string) {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			errc <- err
			return
		}
		q := u.Query()
		switch {
		case q.Get("error") != "":
			errc <- fmt.Errorf("authorization refused: %s", q.Get("error"))
		case q.Get("state") != state:
			errc <- errors.New("state mismatch; start the login again")
		case q.Get("code") == "":
			errc <- errors.New("no code in that URL")
		default:
			select {
			case codes <- q.Get("code"):
			default:
			}
		}
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept(r.URL.String())
		fmt.Fprintln(w, "sotot-gmail: done. You can close this tab.")
	}))
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				accept(line)
			}
		}
	}()

	fmt.Printf(`Open this URL in a browser and approve read-only access for sotot:

%s

Google will warn that it hasn't verified the app: choose Advanced, then
"Go to ... (unsafe)". That's expected for your own unpublished app.

If your browser is on this machine, that's all. Otherwise the final page
won't load (it redirects to 127.0.0.1 on the browser's machine): copy the
URL from the address bar and paste it here.
`, authURL)

	var code string
	select {
	case code = <-codes:
	case err := <-errc:
		return err
	case <-time.After(10 * time.Minute):
		return errors.New("timed out waiting for authorization")
	}
	ctx := context.Background()
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("exchanging code: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("Google returned no refresh token; remove sotot's access at https://myaccount.google.com/connections and log in again")
	}
	data := tokenFileData{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RefreshToken: tok.RefreshToken}
	gm := &gmailClient{hc: cfg.Client(ctx, tok), base: "https://gmail.googleapis.com"}
	if data.Email, err = gm.emailAddress(ctx); err != nil {
		return fmt.Errorf("authorized, but reading Gmail failed: %w", err)
	}
	if err := writeSecret(*out, data); err != nil {
		return err
	}
	fmt.Printf("\nAuthorized read-only access to %s; wrote %s.\n", data.Email, *out)
	return nil
}

// writeSecret atomically writes v as JSON to an owner-only file.
func writeSecret(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sotot-gmail-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
