package curator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeHF serves /api/daily_papers?date=… from per-date bodies (the shape of
// https://huggingface.co/api/daily_papers, read 2026-11-08: an array of
// {title, publishedAt, summary, paper: {id, title, summary, authors[{name}],
// publishedAt, submittedOnDailyAt, upvotes, …}, …}).
type fakeHF struct {
	srv    *httptest.Server
	mu     sync.Mutex
	days   map[string]string // date -> JSON body
	status map[string]int    // date -> non-200 status
	dates  []string          // the date of every request, in order
}

func newFakeHF(t *testing.T) *fakeHF {
	h := &fakeHF{days: map[string]string{}, status: map[string]int{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Path != "/api/daily_papers" {
			http.NotFound(w, r)
			return
		}
		d := r.URL.Query().Get("date")
		h.dates = append(h.dates, d)
		if s := h.status[d]; s != 0 {
			w.WriteHeader(s)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		b, ok := h.days[d]
		if !ok {
			b = "[]"
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(b))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

type tPaper struct {
	id, title, summary, published, onDaily string
	authors                                []string
}

func hfJSON(t *testing.T, ps ...tPaper) string {
	t.Helper()
	var out []map[string]any
	for _, p := range ps {
		var as []map[string]any
		for _, a := range p.authors {
			as = append(as, map[string]any{"name": a, "hidden": false})
		}
		paper := map[string]any{"id": p.id, "title": p.title, "summary": p.summary, "authors": as, "upvotes": 42}
		if p.published != "" {
			paper["publishedAt"] = p.published
		}
		if p.onDaily != "" {
			paper["submittedOnDailyAt"] = p.onDaily
		}
		out = append(out, map[string]any{"title": p.title, "publishedAt": p.published, "paper": paper})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hfFeed() FeedConfig { return FeedConfig{Kind: KindHFDaily} }

// hfworld is a one-channel world whose feeds are given; rss feed paths are
// served by w.feeds as usual.
func hfworld(t *testing.T, maxAge int, feeds ...FeedConfig) (*world, *fakeHF, func(*Options)) {
	w := newWorld(t)
	h := newFakeHF(t)
	for i := range feeds {
		if feeds[i].Kind == "" || feeds[i].Kind == KindRSS {
			feeds[i].URL = w.feeds.url(feeds[i].URL)
		}
	}
	c := &Config{Hub: "hub.example", HubURL: w.hub.srv.URL, MaxAgeHours: maxAge, Channels: []Channel{{
		Slug: "news", Title: "News", About: "test channel", Tags: []string{"test"}, MaxPerRun: 10, Feeds: feeds,
	}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	w.cfg = c
	return w, h, func(o *Options) { o.hfBase = h.srv.URL }
}

func TestHFDailyMapsPapers(t *testing.T) {
	w, h, opt := hfworld(t, 48, hfFeed())
	// testNow is 2026-11-01 12:00 UTC: default lookback 2 reads 11-01 and 10-31.
	h.days["2026-11-01"] = hfJSON(t,
		tPaper{id: "2610.40641", title: "Agentic\n  weather <b>forecasts</b>", summary: "Line one\n\tline two‮.",
			published: "2026-10-29T17:58:53.000Z", onDaily: "2026-11-01T00:00:00.000Z",
			authors: []string{"Ada A", "Bo B", "Cy C", "Di D"}},
		tPaper{id: "2610.40641v2", title: "versioned id", published: "2026-11-01T01:00:00.000Z"},
		tPaper{id: "../../evil", title: "path id", published: "2026-11-01T01:00:00.000Z"},
		tPaper{id: "2610.123", title: "short id", published: "2026-11-01T01:00:00.000Z"},
		tPaper{id: "2610.55555", title: "bad date", published: "yesterday", onDaily: "soon"},
	)
	h.days["2026-10-31"] = hfJSON(t,
		tPaper{id: "2610.12345", title: "Tool use at scale", summary: "We study tools.",
			published: "2026-10-31T09:30:00.000Z", authors: []string{"Eve E"}},
	)
	h.days["2026-10-30"] = hfJSON(t, tPaper{id: "2610.99999", title: "out of lookback", published: "2026-10-31T10:00:00.000Z"})
	res := w.run(opt)
	if res.ExitCode() != 0 || res.FeedsOK != 1 {
		t.Fatalf("exit %d ok %d: %v", res.ExitCode(), res.FeedsOK, res.Errors)
	}
	if want := []string{"2026-11-01", "2026-10-31"}; !reflect.DeepEqual(h.dates, want) {
		t.Fatalf("requested dates %v, want %v (one request per date)", h.dates, want)
	}
	want := []string{"https://arxiv.org/abs/2610.12345", "https://arxiv.org/abs/2610.40641"}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, want) {
		t.Fatalf("posted %v\nwant %v (bad ids and bad dates must be skipped)", got, want)
	}
	p := w.hub.posts("news")
	if p[1].Fields["subject"] != "Agentic weather <b>forecasts</b>" ||
		p[1].Fields["body"] != "https://arxiv.org/abs/2610.40641\n\nLine one line two.\n\nvia Hugging Face Daily Papers" {
		t.Errorf("post 2: %q / %q", p[1].Fields["subject"], p[1].Fields["body"])
	}
	if p[0].Fields["body"] != "https://arxiv.org/abs/2610.12345\n\nWe study tools.\n\nvia Hugging Face Daily Papers" {
		t.Errorf("post 1: %q", p[0].Fields["body"])
	}
}

func TestHFItemFields(t *testing.T) {
	body := hfJSON(t, tPaper{id: "2611.05376", title: "", summary: "  Abstract  text ", published: "2026-11-03T18:59:52.000Z",
		onDaily: "2026-11-06T00:00:00.000Z", authors: []string{"Hao Fei", "", "Bo B", "Cy C", "Di D"}})
	var es []hfDailyEntry
	if err := json.Unmarshal([]byte(body), &es); err != nil {
		t.Fatal(err)
	}
	e := es[0]
	e.Title = "Top-level title"
	it, ok := hfItem(e)
	want := Item{GUID: "hfdaily:2611.05376", Link: "https://arxiv.org/abs/2611.05376", Title: "Top-level title",
		Summary: "Abstract text", Date: utc("2026-11-06T00:00:00Z"), Author: "Hao Fei, Bo B, Cy C et al.", Source: "Hugging Face Daily Papers"}
	if !ok || !reflect.DeepEqual(it, want) {
		t.Fatalf("item %+v ok=%v\nwant %+v", it, ok, want)
	}
	// No submittedOnDailyAt: publishedAt.
	e.Paper.SubmittedOnDailyAt = ""
	if it, _ := hfItem(e); !it.Date.Equal(utc("2026-11-03T18:59:52Z")) {
		t.Errorf("date %v, want publishedAt", it.Date)
	}
	for _, id := range []string{"2611.05376v1", "2611.053", "2611.053761", "211.05376", "x2611.05376", "2611.05376\n", ""} {
		e.Paper.ID = id
		if _, ok := hfItem(e); ok {
			t.Errorf("arXiv id %q accepted", id)
		}
	}
}

// The fixed via label: a source label from the item wins over the feed title,
// and the config cannot set a name.
func TestHFDailyViaIsFixed(t *testing.T) {
	if HFDailySource != "Hugging Face Daily Papers" {
		t.Fatalf("HFDailySource = %q: other components match this exact string", HFDailySource)
	}
	f := FeedConfig{Kind: KindHFDaily, Name: "Something else"}
	if err := f.validate(); err == nil || !strings.Contains(err.Error(), "name on an hfdaily source") {
		t.Fatalf("name on hfdaily: %v, want it rejected", err)
	}
}

func TestHFDailyConfigValidation(t *testing.T) {
	good := FeedConfig{Kind: KindHFDaily}
	if err := good.validate(); err != nil || good.LookbackDays != 2 {
		t.Fatalf("default: %v lookback %d", err, good.LookbackDays)
	}
	if f := (FeedConfig{Kind: KindHFDaily, LookbackDays: 7}); f.validate() != nil {
		t.Errorf("lookback_days 7 rejected")
	}
	for i, f := range []FeedConfig{
		{Kind: KindHFDaily, LookbackDays: 8},
		{Kind: KindHFDaily, LookbackDays: -1},
		{Kind: KindHFDaily, URL: "https://huggingface.co/api/daily_papers"},
		{Kind: KindHFDaily, Query: "agents"},
		{Kind: KindHFDaily, LookbackHours: 24},
		{Kind: KindHFDaily, ChannelID: testChannel},
		{Kind: KindHFDaily, TitleFromSummary: true},
		{Kind: KindRSS, URL: "https://x.example", LookbackDays: 2},
		{Kind: KindXSearch, Query: "q", LookbackDays: 2},
		{Kind: KindDiscord, ChannelID: testChannel, GuildID: testGuild, LookbackDays: 2},
	} {
		if err := f.validate(); err == nil {
			t.Errorf("bad[%d] %+v validated", i, f)
		}
	}
}

// One date failing is a WARN and the others still post; every date failing
// fails the feed (a WARN, like a failed rss feed), not the run.
func TestHFDailyHTTPErrors(t *testing.T) {
	w, h, opt := hfworld(t, 48, hfFeed())
	h.status["2026-11-01"] = http.StatusInternalServerError
	h.days["2026-10-31"] = hfJSON(t, tPaper{id: "2610.12345", title: "t", published: "2026-10-31T09:30:00.000Z"})
	res := w.run(opt)
	if res.FeedsOK != 1 || len(w.hub.posts("news")) != 1 || !strings.Contains(w.out.String(), "WARN channel=news hfdaily date 2026-11-01 failed: HTTP 500") {
		t.Fatalf("ok %d posts %d: one failed date must not drop the other", res.FeedsOK, len(w.hub.posts("news")))
	}

	// Another source in the channel keeps the run at exit 0, as with rss.
	w2, h2, opt2 := hfworld(t, 48, hfFeed(), feed("/a"))
	w2.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "one", date: ago(1)}))
	h2.status["2026-11-01"], h2.status["2026-10-31"] = 503, 404
	res = w2.run(opt2)
	if res.FeedsFailed != 1 || res.FeedsOK != 1 || res.ExitCode() != 0 || len(res.Errors) != 0 {
		t.Fatalf("failed %d ok %d exit %d errors %v", res.FeedsFailed, res.FeedsOK, res.ExitCode(), res.Errors)
	}
	if !strings.Contains(w2.out.String(), "WARN channel=news feed hfdaily failed: date 2026-11-01: HTTP 503") {
		t.Errorf("want the feed-failed WARN")
	}
}

// The same paper from the arXiv API feed (testdata/arxiv.atom links
// https://arxiv.org/abs/2610.40641v1) and from hfdaily has one seen key.
func TestHFDailyDedupesWithArxiv(t *testing.T) {
	atom, err := os.ReadFile("testdata/arxiv.atom")
	if err != nil {
		t.Fatal(err)
	}
	af, err := ParseFeed(atom)
	if err != nil {
		t.Fatal(err)
	}
	hi, ok := hfItem(hfDailyEntry{Paper: hfPaper{ID: "2610.40641", PublishedAt: "2026-10-29T17:58:53Z"}})
	if !ok {
		t.Fatal("hf item skipped")
	}
	if a, b := keyFromLine(NormalizeLink(af.Items[0].Link)), keyFromLine(NormalizeLink(hi.Link)); a == "" || a != b {
		t.Fatalf("seen keys differ: arXiv %q, hfdaily %q", a, b)
	}

	// End to end, either config order: one post, credited to the first feed.
	for _, hfFirst := range []bool{false, true} {
		feeds := []FeedConfig{{URL: "/arxiv", Name: "arXiv"}, hfFeed()}
		wantVia := "via arXiv"
		if hfFirst {
			feeds[0], feeds[1] = feeds[1], feeds[0]
			wantVia = "via Hugging Face Daily Papers"
		}
		w, h, opt := hfworld(t, 120, feeds...)
		w.feeds.set("/arxiv", string(atom))
		h.days["2026-11-01"] = hfJSON(t, tPaper{id: "2610.40641", title: "From extreme weather", summary: "s",
			published: "2026-10-29T17:58:53.000Z", onDaily: "2026-11-01T00:00:00.000Z"})
		w.run(opt)
		var n int
		for _, p := range w.hub.posts("news") {
			if strings.HasPrefix(p.Fields["body"].(string), "https://arxiv.org/abs/2610.40641\n") {
				n++
				if !strings.HasSuffix(p.Fields["body"].(string), wantVia) {
					t.Errorf("hfFirst=%v: body %q, want %s", hfFirst, p.Fields["body"], wantVia)
				}
			}
		}
		if n != 1 {
			t.Fatalf("hfFirst=%v: paper 2610.40641 posted %d times, want once (arXiv and hfdaily must share a key)", hfFirst, n)
		}
	}
	// A later version in the arXiv feed is the same key too.
	if a, b := NormalizeLink("https://arxiv.org/abs/2610.40000v2"), NormalizeLink(hfLink("2610.40000")); a != b || a != "https://arxiv.org/abs/2610.40000" {
		t.Errorf("v2 %q, hfdaily %q: want both https://arxiv.org/abs/2610.40000", a, b)
	}
}

func TestHFDailyDisabledAndFilters(t *testing.T) {
	off := hfFeed()
	off.Disabled = true
	w, h, opt := hfworld(t, 48, off)
	w.run(opt)
	if len(h.dates) != 0 {
		t.Fatalf("a disabled hfdaily source made %d requests", len(h.dates))
	}
	f := hfFeed()
	f.Include, f.Exclude, f.MaxPerRun = []string{"agent"}, []string{"survey"}, 1
	w, h, opt = hfworld(t, 48, f)
	h.days["2026-11-01"] = hfJSON(t,
		tPaper{id: "2611.00001", title: "Agents A", published: "2026-11-01T01:00:00.000Z"},
		tPaper{id: "2611.00002", title: "Agents B", published: "2026-11-01T02:00:00.000Z"},
		tPaper{id: "2611.00003", title: "An agent survey", published: "2026-11-01T03:00:00.000Z"},
		tPaper{id: "2611.00004", title: "Diffusion", published: "2026-11-01T04:00:00.000Z"},
	)
	w.run(opt)
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://arxiv.org/abs/2611.00002"}) {
		t.Fatalf("posted %v, want only the newest included, non-excluded paper (max_per_run 1)", got)
	}
}
