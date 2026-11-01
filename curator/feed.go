package curator

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxFeedBytes caps one feed response. A larger body fails that feed.
	MaxFeedBytes = 5 << 20
	// FeedTimeout bounds one feed fetch, connection to last byte.
	FeedTimeout = 20 * time.Second
	// maxItemsPerFeed bounds how many items of one feed are considered.
	maxItemsPerFeed = 1000
)

// Item is one feed entry, already reduced to plain text. Link is the raw
// link as the feed gave it (NormalizeLink decides whether it is usable). A
// zero Date means the item is undated.
type Item struct {
	GUID    string
	Link    string
	Title   string
	Summary string
	Date    time.Time
}

// Feed is a parsed RSS 2.0, RSS 1.0 (RDF) or Atom document.
type Feed struct {
	Title string
	Items []Item
}

// FetchFeed GETs url and parses it. Any non-200 status, a body over
// MaxFeedBytes or a parse failure is an error for this feed only.
func FetchFeed(ctx context.Context, client *http.Client, url string) (*Feed, error) {
	ctx, cancel := context.WithTimeout(ctx, FeedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFeedBytes {
		return nil, fmt.Errorf("feed exceeds %d bytes", MaxFeedBytes)
	}
	return ParseFeed(body)
}

// ParseFeed parses an RSS 2.0, RSS 1.0 (RDF) or Atom document.
func ParseFeed(data []byte) (*Feed, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	d.Entity = xml.HTMLEntity // &nbsp; and friends appear in real feeds
	d.CharsetReader = charsetReader
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no root element")
			}
			return nil, fmt.Errorf("parse: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "rss":
			var doc struct {
				Channel struct {
					Title textNode  `xml:"title"`
					Items []rssItem `xml:"item"`
				} `xml:"channel"`
			}
			if err := d.DecodeElement(&doc, &start); err != nil {
				return nil, fmt.Errorf("parse rss: %w", err)
			}
			return rssFeed(doc.Channel.Title, doc.Channel.Items), nil
		case "RDF":
			var doc struct {
				Channel struct {
					Title textNode `xml:"title"`
				} `xml:"channel"`
				Items []rssItem `xml:"item"`
			}
			if err := d.DecodeElement(&doc, &start); err != nil {
				return nil, fmt.Errorf("parse rdf: %w", err)
			}
			return rssFeed(doc.Channel.Title, doc.Items), nil
		case "feed":
			var doc struct {
				Title   textNode    `xml:"title"`
				Entries []atomEntry `xml:"entry"`
			}
			if err := d.DecodeElement(&doc, &start); err != nil {
				return nil, fmt.Errorf("parse atom: %w", err)
			}
			return atomFeed(doc.Title, doc.Entries), nil
		default:
			return nil, fmt.Errorf("not a feed: root element <%s>", start.Name.Local)
		}
	}
}

// charsetReader accepts the encodings real feeds declare besides UTF-8.
// Windows-1252 is read as Latin-1, which differs only in 0x80–0x9F.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		raw, err := io.ReadAll(io.LimitReader(in, MaxFeedBytes+1))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, 0, len(raw)*2)
		for _, c := range raw {
			buf = utf8.AppendRune(buf, rune(c))
		}
		return bytes.NewReader(buf), nil
	}
	return nil, fmt.Errorf("unsupported charset %q", label)
}

// textNode collects the character data of an element and of everything
// inside it (so Atom type="xhtml" reduces to its text), plus its type attribute.
type textNode struct {
	Type string
	Text string
}

func (t *textNode) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, a := range start.Attr {
		if a.Name.Local == "type" {
			t.Type = strings.ToLower(a.Value)
		}
	}
	var b strings.Builder
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case xml.CharData:
			if b.Len() < maxRawField {
				b.Write(v)
			}
		case xml.StartElement:
			depth++
			b.WriteByte(' ')
		case xml.EndElement:
			if depth == 0 {
				t.Text = b.String()
				return nil
			}
			depth--
			b.WriteByte(' ')
		}
	}
}

// atomText is the plain text of an Atom text construct (RFC 4287 §3.1).
func (t textNode) atomText() string {
	if t.Type == "html" || t.Type == "text/html" {
		return StripHTML(t.Text)
	}
	return PlainText(t.Text) // "text", "xhtml" (already reduced to text), absent
}

const atomNS = "http://www.w3.org/2005/Atom"

type rssLink struct {
	XMLName xml.Name
	Href    string `xml:"href,attr"`
	Text    string `xml:",chardata"`
}

type rssItem struct {
	Title       textNode  `xml:"title"`
	Links       []rssLink `xml:"link"`
	GUID        string    `xml:"guid"`
	About       string    `xml:"http://www.w3.org/1999/02/22-rdf-syntax-ns# about,attr"`
	PubDate     string    `xml:"pubDate"`
	DCDate      string    `xml:"http://purl.org/dc/elements/1.1/ date"`
	Description textNode  `xml:"description"`
	Content     textNode  `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
}

func rssFeed(title textNode, items []rssItem) *Feed {
	f := &Feed{Title: StripHTML(title.Text)}
	for i, it := range items {
		if i == maxItemsPerFeed {
			break
		}
		var link string
		for _, l := range it.Links {
			// Prefer the RSS <link> text; an <atom:link href> is a fallback.
			if s := strings.TrimSpace(l.Text); s != "" && l.XMLName.Space != atomNS {
				link = s
				break
			}
		}
		if link == "" {
			for _, l := range it.Links {
				if s := strings.TrimSpace(l.Href); s != "" {
					link = s
					break
				}
			}
		}
		guid := strings.TrimSpace(it.GUID)
		if guid == "" {
			guid = strings.TrimSpace(it.About)
		}
		date := parseDate(it.PubDate)
		if date.IsZero() {
			date = parseDate(it.DCDate)
		}
		// RSS titles and descriptions are HTML by convention.
		summary := StripHTML(it.Description.Text)
		if summary == "" {
			summary = StripHTML(it.Content.Text)
		}
		f.Items = append(f.Items, Item{
			GUID: guid, Link: link, Title: StripHTML(it.Title.Text), Summary: summary, Date: date,
		})
	}
	return f
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     textNode   `xml:"title"`
	Links     []atomLink `xml:"link"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Summary   textNode   `xml:"summary"`
	Content   textNode   `xml:"content"`
}

// atomLinkOf picks the entry's alternate link: rel="alternate" (or no rel),
// preferring type text/html, else the first link with an href.
func atomLinkOf(links []atomLink) string {
	best := ""
	for _, l := range links {
		href := strings.TrimSpace(l.Href)
		if href == "" {
			continue
		}
		if l.Rel == "" || l.Rel == "alternate" {
			if l.Type == "" || strings.HasPrefix(l.Type, "text/html") {
				return href
			}
			if best == "" {
				best = href
			}
		}
	}
	if best == "" {
		for _, l := range links {
			if href := strings.TrimSpace(l.Href); href != "" {
				return href
			}
		}
	}
	return best
}

func atomFeed(title textNode, entries []atomEntry) *Feed {
	f := &Feed{Title: title.atomText()}
	for i, e := range entries {
		if i == maxItemsPerFeed {
			break
		}
		date := parseDate(e.Published)
		if date.IsZero() {
			date = parseDate(e.Updated)
		}
		summary := e.Summary.atomText()
		if summary == "" {
			summary = e.Content.atomText()
		}
		f.Items = append(f.Items, Item{
			GUID: strings.TrimSpace(e.ID), Link: atomLinkOf(e.Links),
			Title: e.Title.atomText(), Summary: summary, Date: date,
		})
	}
	return f
}

// zoneOffsets maps the RFC 822 zone names (and the few US ones feeds use) to
// numeric offsets. time.Parse would otherwise resolve an abbreviation against
// the host's local zone, so the same feed could parse differently per host.
var zoneOffsets = map[string]string{
	"UT": "+0000", "UTC": "+0000", "GMT": "+0000", "Z": "+0000",
	"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500",
	"MST": "-0700", "MDT": "-0600", "PST": "-0800", "PDT": "-0700",
}

var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04Z07:00",
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04 -0700",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04 -0700",
	"Monday, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 January 2006 15:04:05 -0700",
	"2006-01-02",
}

// parseDate parses the RFC 3339 and RFC 822 forms feeds use. It returns the
// zero time when the string is empty or no layout fits.
func parseDate(s string) time.Time {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return time.Time{}
	}
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		zone := s[i+1:]
		if off, ok := zoneOffsets[strings.ToUpper(zone)]; ok {
			s = s[:i+1] + off
		} else if isAlpha(zone) {
			return time.Time{} // an unknown zone name: no trustworthy date
		}
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func isAlpha(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return s != ""
}
