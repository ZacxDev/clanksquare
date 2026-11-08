package curator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// hfdaily: Hugging Face Daily Papers, read from the JSON API
// GET https://huggingface.co/api/daily_papers?date=YYYY-MM-DD (one request
// per UTC date, today and the lookback_days-1 days before it). Hugging Face
// publishes no RSS/Atom feed for Daily Papers (huggingface.co/papers/rss and
// friends answer 401/404, read 2026-11-08), hence a kind of its own. The
// response is untrusted data: a paper is kept only if its id is an arXiv id,
// and its text is reduced to plain text and bounded like feed text.

const (
	hfDefaultBase = "https://huggingface.co"
	// HFDailySource is the via label of every hfdaily item. It is fixed (a
	// feed's name cannot override it) because other components recognise
	// Hugging Face Daily Papers items by this exact string.
	HFDailySource       = "Hugging Face Daily Papers"
	defaultLookbackDays = 2
	maxLookbackDays     = 7
	// maxHFAuthors: more authors than this are shortened to "et al.".
	maxHFAuthors = 3
)

// arxivIDRe is the post-2007 arXiv identifier, without a version suffix.
var arxivIDRe = regexp.MustCompile(`^\d{4}\.\d{4,5}$`)

// hfLink is the link of the arXiv paper id, without a version: NormalizeLink
// maps every version of an arXiv abs/pdf/html link to this form, so a paper
// posted from both an arXiv API feed (at any version) and hfdaily has one
// dedupe key.
func hfLink(id string) string { return "https://arxiv.org/abs/" + id }

type hfDailyEntry struct {
	Title string  `json:"title"`
	Paper hfPaper `json:"paper"`
}

type hfPaper struct {
	ID                 string     `json:"id"`
	Title              string     `json:"title"`
	Summary            string     `json:"summary"`
	PublishedAt        string     `json:"publishedAt"`
	SubmittedOnDailyAt string     `json:"submittedOnDailyAt"`
	Authors            []hfAuthor `json:"authors"`
}

type hfAuthor struct {
	Name string `json:"name"`
}

// hfDates are the UTC dates read: today first, then each day before it.
func hfDates(now time.Time, days int) []string {
	now = now.UTC()
	out := make([]string, 0, days)
	for i := 0; i < days; i++ {
		out = append(out, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

func (r *runner) fetchHFDaily(ctx context.Context, ch *Channel, fc FeedConfig) (*Feed, error) {
	base := r.hfBase
	if base == "" {
		base = hfDefaultBase
	}
	f := &Feed{Title: HFDailySource}
	var firstErr error
	ok := 0
	for _, d := range hfDates(r.Now(), fc.LookbackDays) {
		entries, err := r.hfGet(ctx, base+"/api/daily_papers?date="+url.QueryEscape(d))
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("date %s: %w", d, err)
			}
			r.logf("WARN", "channel=%s hfdaily date %s failed: %v", ch.Slug, d, err)
			continue
		}
		ok++
		for _, e := range entries {
			if len(f.Items) >= maxItemsPerFeed {
				break
			}
			if it, keep := hfItem(e); keep {
				f.Items = append(f.Items, it)
			}
		}
	}
	if ok == 0 {
		return nil, firstErr // every date failed: the feed failed
	}
	return f, nil
}

func (r *runner) hfGet(ctx context.Context, u string) ([]hfDailyEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, FeedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if len(body) > MaxFeedBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", MaxFeedBytes)
	}
	var entries []hfDailyEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("response: %w", err)
	}
	return entries, nil
}

// hfItem maps one paper to an item. It is skipped unless its id is an arXiv
// id and it has a parseable date (submittedOnDailyAt, else publishedAt).
func hfItem(e hfDailyEntry) (Item, bool) {
	p := e.Paper
	if !arxivIDRe.MatchString(p.ID) {
		return Item{}, false
	}
	var date time.Time
	for _, s := range []string{p.SubmittedOnDailyAt, p.PublishedAt} {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			date = t.UTC()
			break
		}
	}
	if date.IsZero() {
		return Item{}, false
	}
	title := PlainText(p.Title)
	if title == "" {
		title = PlainText(e.Title)
	}
	var names []string
	for _, a := range p.Authors {
		if n := PlainText(a.Name); n != "" {
			names = append(names, n)
		}
	}
	author := ""
	if len(names) > maxHFAuthors {
		author = strings.Join(names[:maxHFAuthors], ", ") + " et al."
	} else {
		author = strings.Join(names, ", ")
	}
	return Item{
		GUID: "hfdaily:" + p.ID, Link: hfLink(p.ID), Title: Truncate(title, maxSubject),
		Summary: PlainText(p.Summary), Date: date, Author: Truncate(author, maxVia), Source: HFDailySource,
	}, true
}
