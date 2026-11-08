package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

const feedStateTTL = 30 * 24 * time.Hour

type Feed struct {
	ID           string   `json:"id"`
	URL          string   `json:"url"`
	IntervalSecs int      `json:"interval_secs"`
	DeliverTo    string   `json:"deliver_to"`
	Tags         []string `json:"tags"`
}
type FeedState struct {
	FeedID              string               `json:"feed_id"`
	ETag                string               `json:"etag"`
	LastModified        string               `json:"last_modified"`
	LastFetch           time.Time            `json:"last_fetch"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
	NextPoll            time.Time            `json:"next_poll"`
	Seen                map[string]time.Time `json:"seen"`
}
type FeedStateStore interface {
	LoadFeedState(context.Context, string) (FeedState, error)
	SaveFeedState(context.Context, FeedState) error
}
type RSSPoller struct {
	Feeds        []Feed
	Store        FeedStateStore
	Inbox        InboxWriter
	MaxItemBytes int
	Client       *http.Client
	Now          func() time.Time
	Jitter       func(time.Duration) time.Duration
}
type rssDocument struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Items   []rssItem   `xml:"item"`
	Entries []atomEntry `xml:"entry"`
}
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
	Summary     string
}
type atomEntry struct {
	Title     string `xml:"title"`
	ID        string `xml:"id"`
	Updated   string `xml:"updated"`
	Published string `xml:"published"`
	Summary   string `xml:"summary"`
	Content   string `xml:"content"`
	Links     []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
}

func LoadFeeds(path string, knownAgent func(string) bool) ([]Feed, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("CR_INGEST_RSS_CONFIG_FILE is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("CR_INGEST_RSS_CONFIG_FILE: %w", err)
	}
	var feeds []Feed
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&feeds); err != nil {
		return nil, fmt.Errorf("CR_INGEST_RSS_CONFIG_FILE: %w", err)
	}
	var trailing any
	if err = d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("CR_INGEST_RSS_CONFIG_FILE: trailing JSON data")
	}
	if len(feeds) == 0 {
		return nil, fmt.Errorf("CR_INGEST_RSS_CONFIG_FILE empty")
	}
	seen := map[string]bool{}
	for i := range feeds {
		f := &feeds[i]
		if !sourceIDPattern.MatchString(f.ID) {
			return nil, fmt.Errorf("feed %q: invalid id", f.ID)
		}
		if seen[f.ID] {
			return nil, fmt.Errorf("feed %q: duplicate id", f.ID)
		}
		seen[f.ID] = true
		u, e := url.Parse(f.URL)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return nil, fmt.Errorf("feed %q: URL must use https", f.ID)
		}
		if e = rejectUnsafeHost(u.Hostname()); e != nil {
			return nil, fmt.Errorf("feed %q: unsafe URL: %w", f.ID, e)
		}
		if f.IntervalSecs == 0 {
			f.IntervalSecs = 300
		}
		if f.IntervalSecs < 60 {
			return nil, fmt.Errorf("feed %q: interval_secs must be at least 60", f.ID)
		}
		if f.DeliverTo == "" || knownAgent == nil || !knownAgent(f.DeliverTo) {
			return nil, fmt.Errorf("feed %q: unknown deliver_to agent %q", f.ID, f.DeliverTo)
		}
	}
	return feeds, nil
}
func NewSafeFeedClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, e
		}
		for _, ip := range ips {
			if unsafeIP(ip.IP) {
				return nil, fmt.Errorf("unsafe destination address")
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no destination addresses")
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect must use https")
		}
		if e := rejectUnsafeHost(req.URL.Hostname()); e != nil {
			return e
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}}
}
func rejectUnsafeHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if unsafeIP(ip) {
			return fmt.Errorf("private or reserved address")
		}
		return nil
	}
	ips, e := net.LookupIP(host)
	if e != nil {
		return e
	}
	if len(ips) == 0 {
		return fmt.Errorf("host has no addresses")
	}
	for _, ip := range ips {
		if unsafeIP(ip) {
			return fmt.Errorf("private or reserved address")
		}
	}
	return nil
}
func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("fd00:ec2::254"))
}
func (p *RSSPoller) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return NewSafeFeedClient()
}
func (p *RSSPoller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
func (p *RSSPoller) PollOnce(ctx context.Context, f Feed) error {
	s, e := p.Store.LoadFeedState(ctx, f.ID)
	if e != nil {
		return e
	}
	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if e != nil {
		return e
	}
	if s.ETag != "" {
		req.Header.Set("If-None-Match", s.ETag)
	}
	if s.LastModified != "" {
		req.Header.Set("If-Modified-Since", s.LastModified)
	}
	resp, fetchErr := p.client().Do(req)
	now := p.now()
	s.FeedID = f.ID
	s.LastFetch = now
	if fetchErr != nil {
		s.ConsecutiveFailures++
		p.setNext(&s, f)
		_ = p.Store.SaveFeedState(ctx, s)
		return fetchErr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		s.ConsecutiveFailures = 0
		p.setNext(&s, f)
		return p.Store.SaveFeedState(ctx, s)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.ConsecutiveFailures++
		p.setNext(&s, f)
		_ = p.Store.SaveFeedState(ctx, s)
		return fmt.Errorf("feed %s returned HTTP %d", f.ID, resp.StatusCode)
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		s.ConsecutiveFailures++
		p.setNext(&s, f)
		_ = p.Store.SaveFeedState(ctx, s)
		return e
	}
	items, e := parseFeed(body)
	if e != nil {
		s.ConsecutiveFailures++
		p.setNext(&s, f)
		_ = p.Store.SaveFeedState(ctx, s)
		return fmt.Errorf("parse_error: %w", e)
	}
	s.ETag = resp.Header.Get("ETag")
	s.LastModified = resp.Header.Get("Last-Modified")
	s.ConsecutiveFailures = 0
	for _, it := range items {
		id := it.Link
		if id == "" {
			id = it.GUID
		}
		if id == "" {
			sum := sha256.Sum256([]byte(it.Title + it.PubDate))
			id = hex.EncodeToString(sum[:])
		}
		if it.Title == "" && it.PubDate == "" {
			continue
		}
		if _, ok := s.Seen[id]; ok {
			continue
		}
		text := truncateUTF8(cleanFeedText(strings.Join([]string{it.Title, it.Link, it.Summary}, " ")), p.MaxItemBytes)
		payload, _ := json.Marshal(map[string]string{"text": text})
		if p.Inbox != nil {
			_, e = p.Inbox.Deliver(ctx, f.DeliverTo, "", ingress.InboxMessage{Author: session.AuthorRef{System: "feed-" + f.ID}, Kind: session.MessageKind("text"), Text: text, Payload: payload, Tags: append(append([]string{}, f.Tags...), "untrusted:feed"), IdempotencyKey: "rss:" + f.ID + ":" + id})
			if e != nil {
				continue
			}
		}
		s.Seen[id] = now
	}
	pruneSeen(s.Seen, now)
	p.setNext(&s, f)
	return p.Store.SaveFeedState(ctx, s)
}
func (p *RSSPoller) setNext(s *FeedState, f Feed) {
	mult := 1 << min(max(s.ConsecutiveFailures-1, 0), 5)
	d := time.Duration(f.IntervalSecs*mult) * time.Second
	if p.Jitter != nil {
		d = p.Jitter(d)
	} else {
		d += time.Duration((rand.Float64()*0.2 - 0.1) * float64(d))
	}
	s.NextPoll = p.now().Add(d)
}
func (p *RSSPoller) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, f := range p.Feeds {
		feed := f
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				s, _ := p.Store.LoadFeedState(ctx, feed.ID)
				delay := time.Until(s.NextPoll)
				if s.NextPoll.IsZero() || delay < 0 {
					delay = time.Millisecond
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					_ = p.PollOnce(ctx, feed)
				}
			}
		}()
	}
	<-ctx.Done()
	workers.Wait()
}
func parseFeed(b []byte) ([]rssItem, error) {
	var d rssDocument
	if e := xml.Unmarshal(b, &d); e != nil {
		return nil, e
	}
	out := append([]rssItem(nil), d.Channel.Items...)
	out = append(out, d.Items...)
	for _, a := range d.Entries {
		link := ""
		for _, l := range a.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		date := a.Published
		if date == "" {
			date = a.Updated
		}
		sum := a.Summary
		if sum == "" {
			sum = a.Content
		}
		out = append(out, rssItem{Title: a.Title, Link: link, GUID: a.ID, PubDate: date, Summary: sum})
	}
	for i := range out {
		if out[i].Summary == "" {
			out[i].Summary = out[i].Description
		}
	}
	return out, nil
}
func cleanFeedText(s string) string {
	s = html.UnescapeString(s)
	var b strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		n = 4096
	}
	if len(s) <= n {
		return s
	}
	if n < 3 {
		return "…"
	}
	s = s[:n-3]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
func pruneSeen(m map[string]time.Time, now time.Time) {
	for k, t := range m {
		if now.Sub(t) > feedStateTTL {
			delete(m, k)
		}
	}
	if len(m) <= 1000 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].Before(m[keys[j]]) })
	for _, k := range keys[:len(keys)-1000] {
		delete(m, k)
	}
}
