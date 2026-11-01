package curator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

var testNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func ago(h float64) string {
	return testNow.Add(-time.Duration(h * float64(time.Hour))).Format(time.RFC3339)
}

type world struct {
	t     *testing.T
	hub   *fakeHub
	feeds *feedServer
	key   []byte
	dir   string
	cfg   *Config
	now   time.Time
	out   bytes.Buffer
	ctx   context.Context // nil: context.Background()
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, hub: newFakeHub(t), feeds: newFeedServer(t), dir: t.TempDir(), now: testNow}
	return w
}

// config builds a one-channel config over the given feed paths and validates it.
func (w *world) config(maxPerRun int, feeds ...FeedConfig) *Config {
	for i := range feeds {
		feeds[i].URL = w.feeds.url(feeds[i].URL)
	}
	c := &Config{Hub: "hub.example", HubURL: w.hub.srv.URL, Channels: []Channel{{
		Slug: "news", Title: "News", About: "test channel", Tags: []string{"test"},
		MaxPerRun: maxPerRun, Feeds: feeds,
	}}}
	if err := c.Validate(); err != nil {
		w.t.Fatal(err)
	}
	w.cfg = c
	return c
}

func (w *world) statePath() string { return filepath.Join(w.dir, "state.json") }

func (w *world) run(mods ...func(*Options)) *Result {
	w.t.Helper()
	w.out.Reset()
	o := Options{Config: w.cfg, Key: testKey("curator"), StatePath: w.statePath(), Client: w.hub.srv.Client(),
		Now: func() time.Time { return w.now }, Out: &w.out}
	for _, m := range mods {
		m(&o)
	}
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res := Run(ctx, o)
	w.t.Logf("run output:\n%s", w.out.String())
	return res
}

func feed(path string) FeedConfig { return FeedConfig{URL: path} }

// ---- selection (pure) ----

func sel(t *testing.T, ch Channel, seen map[string]int64, feeds ...*Feed) ([]candidate, ChannelStats) {
	t.Helper()
	if ch.MaxPerRun == 0 {
		ch.MaxPerRun = 5
	}
	if ch.MaxAgeHours == 0 {
		ch.MaxAgeHours = 48
	}
	var ff []fetchedFeed
	for i, f := range feeds {
		cfg := FeedConfig{URL: "https://feed.example/" + string(rune('a'+i))}
		if i < len(ch.Feeds) {
			cfg = ch.Feeds[i]
		}
		ff = append(ff, fetchedFeed{cfg, f})
	}
	var st ChannelStats
	if seen == nil {
		seen = map[string]int64{}
	}
	return selectItems(&ch, ff, seen, testNow, &st), st
}

func at(h float64) time.Time { return testNow.Add(-time.Duration(h * float64(time.Hour))) }

func lines(cs []candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.line)
	}
	return out
}

func TestSelectNewestNPostedOldestFirst(t *testing.T) {
	f := &Feed{Title: "F", Items: []Item{
		{Link: "https://x.example/3", Title: "three", Date: at(3)},
		{Link: "https://x.example/1", Title: "one", Date: at(1)},
		{Link: "https://x.example/5", Title: "five", Date: at(5)},
		{Link: "https://x.example/2", Title: "two", Date: at(2)},
		{Link: "https://x.example/4", Title: "four", Date: at(4)},
	}}
	got, st := sel(t, Channel{MaxPerRun: 3}, nil, f)
	want := []string{"https://x.example/3", "https://x.example/2", "https://x.example/1"}
	if !reflect.DeepEqual(lines(got), want) {
		t.Fatalf("selected %v, want the newest 3 oldest-first %v", lines(got), want)
	}
	if st.New != 5 || st.Items != 5 {
		t.Errorf("stats %+v: want items=5 new=5 (overflow counts as new, is not posted)", st)
	}
}

func TestSelectTiesAreDeterministic(t *testing.T) {
	same := at(1)
	a := &Feed{Items: []Item{{Link: "https://x.example/b", Date: same}, {Link: "https://x.example/a", Date: same}, {Link: "https://x.example/c", Date: same}}}
	got1, _ := sel(t, Channel{MaxPerRun: 2}, nil, a)
	if !reflect.DeepEqual(lines(got1), []string{"https://x.example/a", "https://x.example/b"}) {
		t.Fatalf("tie order %v: want a and b selected (lower keys win the tie), posted in key order", lines(got1))
	}
}

func TestSelectAgeAndUndated(t *testing.T) {
	f := &Feed{Items: []Item{
		{Link: "https://x.example/fresh", Date: at(47)},
		{Link: "https://x.example/old", Date: at(49)},
		{Link: "https://x.example/undated"},                   // no date: skipped, never posted
		{Link: "https://x.example/future", Date: at(-25)},     // >24h ahead: untrustworthy date
		{Link: "https://x.example/near-future", Date: at(-1)}, // within slack: fine
	}}
	got, st := sel(t, Channel{MaxPerRun: 10}, nil, f)
	if !reflect.DeepEqual(lines(got), []string{"https://x.example/fresh", "https://x.example/near-future"}) {
		t.Fatalf("selected %v", lines(got))
	}
	if st.TooOld != 1 || st.Undated != 2 {
		t.Errorf("stats %+v: want too_old=1 undated=2", st)
	}
	// A per-channel max age overrides the default.
	got, _ = sel(t, Channel{MaxPerRun: 10, MaxAgeHours: 72}, nil, f)
	if len(got) != 3 {
		t.Errorf("max_age_hours=72 selected %v, want the 49h-old item too", lines(got))
	}
}

func TestSelectUndatedColdStart(t *testing.T) {
	// A feed whose items carry no dates must post nothing, ever: on a cold
	// start it would otherwise flood the channel with its whole history.
	var items []Item
	for i := 0; i < 30; i++ {
		items = append(items, Item{Link: "https://x.example/" + string(rune('a'+i)), Title: "t"})
	}
	got, st := sel(t, Channel{MaxPerRun: 50}, nil, &Feed{Items: items})
	if len(got) != 0 || st.Undated != 30 {
		t.Fatalf("undated feed selected %d items (stats %+v), want 0 and undated=30", len(got), st)
	}
}

func TestSelectIncludeExclude(t *testing.T) {
	f := &Feed{Items: []Item{
		{Link: "https://x.example/1", Title: "New Agents framework", Date: at(1)},
		{Link: "https://x.example/2", Title: "Chemistry", Summary: "about reagents", Date: at(1)},
		{Link: "https://x.example/3", Title: "Plain", Summary: "TOOL USE benchmark", Date: at(1)},
		{Link: "https://x.example/4", Title: "Agent crypto token", Date: at(1)},
	}}
	ch := Channel{MaxPerRun: 10, Feeds: []FeedConfig{{Include: []string{"agent", "tool use"}, Exclude: []string{"crypto"}}}}
	got, st := sel(t, ch, nil, f)
	if !reflect.DeepEqual(lines(got), []string{"https://x.example/1", "https://x.example/3"}) {
		t.Fatalf("selected %v", lines(got))
	}
	if st.Filtered != 2 {
		t.Errorf("filtered = %d, want 2", st.Filtered)
	}
}

func TestSelectCrossFeedDedupe(t *testing.T) {
	a := &Feed{Title: "A", Items: []Item{{Link: "https://x.example/same?utm_source=a", Title: "from A", Date: at(1)}}}
	b := &Feed{Title: "B", Items: []Item{
		{Link: "https://X.example/same#frag", Title: "from B", Date: at(1)},
		{Link: "https://x.example/other", Title: "other", Date: at(2)},
	}}
	got, st := sel(t, Channel{MaxPerRun: 10}, nil, a, b)
	if len(got) != 2 || got[1].line != "https://x.example/same" || got[1].via != "A" {
		t.Fatalf("selected %+v: want the shared link once, from the first feed", got)
	}
	if st.Seen != 1 {
		t.Errorf("seen = %d, want 1 (the duplicate)", st.Seen)
	}
	// A configured name replaces the feed title in the via line.
	got, _ = sel(t, Channel{MaxPerRun: 10, Feeds: []FeedConfig{{Name: "Named"}}}, nil, a)
	if got[0].via != "Named" {
		t.Errorf("via = %q, want the configured name", got[0].via)
	}
}

func TestSelectSkipsSeen(t *testing.T) {
	f := &Feed{Items: []Item{{Link: "https://x.example/1", Date: at(1)}, {Link: "https://x.example/2", Date: at(1)}}}
	got, st := sel(t, Channel{MaxPerRun: 10}, map[string]int64{"https://x.example/1": 1}, f)
	if !reflect.DeepEqual(lines(got), []string{"https://x.example/2"}) || st.Seen != 1 {
		t.Fatalf("selected %v stats %+v", lines(got), st)
	}
}

func TestSelectGuidFallback(t *testing.T) {
	f := &Feed{Items: []Item{
		{GUID: "https://x.example/perma", Date: at(1)}, // a URL guid is the link
		{GUID: "urn:uuid:1234", Date: at(2)},           // otherwise the guid itself
		{Date: at(3)},                                  // neither: unusable
	}}
	// Guids whose cleaned-up form is a URL although the raw one is not.
	odd := &Feed{Items: []Item{
		{GUID: "https://x.example/del\x7f", Date: at(1)},
		{GUID: "https://x.example/" + strings.Repeat("a", maxLink), Date: at(1)},
	}}
	oddGot, _ := sel(t, Channel{MaxPerRun: 10}, nil, odd)
	if len(oddGot) != 2 {
		t.Fatalf("odd guids selected %d", len(oddGot))
	}
	for _, c := range oddGot {
		if keyFromLine(c.line) != c.key {
			t.Errorf("keyFromLine(%.40q) = %.50q, want %.50q: a rebuild would not recognise this post", c.line, keyFromLine(c.line), c.key)
		}
	}
	got, st := sel(t, Channel{MaxPerRun: 10}, nil, f)
	if !reflect.DeepEqual(lines(got), []string{"urn:uuid:1234", "https://x.example/perma"}) {
		t.Fatalf("selected %v", lines(got))
	}
	if got[0].key != "guid:urn:uuid:1234" || st.Filtered != 1 {
		t.Errorf("key %q, stats %+v", got[0].key, st)
	}
	for _, c := range got {
		if keyFromLine(c.line) != c.key {
			t.Errorf("keyFromLine(%q) = %q, want %q: a rebuild would not recognise this post", c.line, keyFromLine(c.line), c.key)
		}
	}
}

func TestFormatPost(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	subject, body := formatPost(candidate{line: "https://x.example/p", title: "", summary: long, via: "Feed"})
	if subject != "x.example" {
		t.Errorf("subject = %q, want the host fallback", subject)
	}
	parts := strings.Split(body, "\n\n")
	if len(parts) != 3 || parts[0] != "https://x.example/p" || parts[2] != "via Feed" {
		t.Fatalf("body = %q", body)
	}
	if len(parts[1]) > maxExcerpt || !strings.HasSuffix(parts[1], "…") {
		t.Errorf("excerpt is %d bytes", len(parts[1]))
	}
	_, body = formatPost(candidate{line: "https://x.example/p", title: "T", via: "Feed"})
	if body != "https://x.example/p\n\nvia Feed" {
		t.Errorf("empty excerpt: body = %q, want the paragraph omitted", body)
	}
	subject, _ = formatPost(candidate{line: "https://x.example/p", title: strings.Repeat("☺", 100)})
	if len(subject) > maxSubject {
		t.Errorf("subject is %d bytes", len(subject))
	}
}

// ---- full runs against the fake hub ----

func TestRunPostsThenNothingNew(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("Feed A",
		entry{id: "1", link: "https://x.example/1", title: "One <b>bold</b>", summary: "&lt;p&gt;Hello &amp;amp; bye&lt;/p&gt;", date: ago(3)},
		entry{id: "2", link: "https://x.example/2", title: "Two", date: ago(2)},
	))
	w.config(5, feed("/a"))
	res := w.run()
	if res.ExitCode() != 0 {
		t.Fatalf("run 1 exit %d: %v", res.ExitCode(), res.Errors)
	}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1", "https://x.example/2"}) {
		t.Fatalf("run 1 posted %v", got)
	}
	p := w.hub.posts("news")[0]
	if p.Fields["subject"] != "One bold" || p.Fields["body"] != "https://x.example/1\n\nHello & bye\n\nvia Feed A" {
		t.Errorf("post fields: subject %q body %q", p.Fields["subject"], p.Fields["body"])
	}
	if p.From != keyID(testKey("curator")) {
		t.Errorf("post from %s", p.From)
	}
	if !strings.Contains(w.out.String(), "channel=news feeds_ok=1 feeds_failed=0 items=2 new=2 posted=2 seen=0 too_old=0 undated=0 filtered=0") {
		t.Errorf("summary line missing or wrong")
	}
	w.now = w.now.Add(time.Hour)
	res = w.run()
	if res.ExitCode() != 0 || len(w.hub.posts("news")) != 2 {
		t.Fatalf("run 2: exit %d, %d posts (want 0 new)", res.ExitCode(), len(w.hub.posts("news")))
	}
	if !strings.Contains(w.out.String(), "posted=0 seen=2") {
		t.Errorf("run 2 summary should report posted=0 seen=2")
	}
	if w.hub.count("channel") != 1 {
		t.Errorf("channel objects submitted: %d, want 1 (no update without drift)", w.hub.count("channel"))
	}
}

func TestRunStateWrittenAfterEachPost(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(3)},
		entry{id: "2", link: "https://x.example/2", title: "2", date: ago(2)},
		entry{id: "3", link: "https://x.example/3", title: "3", date: ago(1)},
	))
	w.config(5, feed("/a"))
	// Crash right after the first post's submit (and state write).
	func() {
		defer func() { recover() }()
		w.run(func(o *Options) {
			o.afterPost = func(string) { panic("simulated crash") }
		})
	}()
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("before the crash %d posts were made, want 1", n)
	}
	st, err := LoadState(w.statePath())
	if err != nil || st == nil {
		t.Fatalf("state after crash: %v, %v", st, err)
	}
	cs := st.Channels[w.hub.slugs["news"]]
	if cs == nil || len(cs.Seen) != 1 || cs.Seen["https://x.example/1"] == 0 {
		t.Fatalf("state after crash does not hold the post that was made: %+v", cs)
	}
	// The next run finishes the job without reposting the first item.
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1", "https://x.example/2", "https://x.example/3"}) {
		t.Fatalf("after recovery the log is %v, want each item exactly once", got)
	}
}

func TestRunRebuildsSeenFromLog(t *testing.T) {
	w := newWorld(t)
	es := []entry{
		{id: "a", link: "https://x.example/a", title: "a", date: ago(4)},
		{id: "b", link: "https://x.example/b", title: "b", date: ago(3)},
		{id: "c", link: "https://x.example/c", title: "c", date: ago(2)},
		{id: "d", link: "https://x.example/d", title: "d", date: ago(1)},
	}
	w.feeds.set("/a", atomFeedXML("A", es...))
	w.config(2, feed("/a"))
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/c", "https://x.example/d"}) {
		t.Fatalf("run 1 posted %v", got)
	}
	cid := w.hub.slugs["news"]
	// Another key's post of item a, and a token claiming to be ours with a
	// broken signature carrying item b. Neither may count as "ours".
	other := testKey("other")
	w.hub.appendLog("news", clankmail.Sign(other, msgPayload(other, cid, "a", "https://x.example/a\n\nvia x", testNow.UnixMilli())))
	forged := clankmail.Sign(testKey("curator"), msgPayload(testKey("curator"), cid, "b", "https://x.example/b\n\nvia x", testNow.UnixMilli()))
	forged = forged[:len(forged)-2] + flip(forged[len(forged)-2:])
	if _, err := clankmail.Verify(forged); err == nil {
		t.Fatal("forged token verifies; the test is broken")
	}
	w.hub.appendLog("news", forged)

	if err := os.Remove(w.statePath()); err != nil {
		t.Fatal(err)
	}
	w.cfg.Channels[0].MaxPerRun = 10
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	var ours []string
	for _, p := range w.hub.posts("news") {
		if p.From == keyID(testKey("curator")) {
			line, _, _ := strings.Cut(p.Fields["body"].(string), "\n")
			ours = append(ours, line)
		}
	}
	// c and d come back from the log; a and b were never posted by this key.
	want := []string{"https://x.example/c", "https://x.example/d", "https://x.example/a", "https://x.example/b"}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("after rebuild our posts are %v, want %v", ours, want)
	}
	if !strings.Contains(w.out.String(), "seen=2") {
		t.Errorf("rebuild should have reported seen=2")
	}
}

// flip changes the first character of a base64url string to another valid one.
func flip(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

func TestRunCorruptStateRebuilds(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	os.WriteFile(w.statePath(), []byte("{not json"), 0o600)
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("a corrupt state file caused a repost: %d posts", n)
	}
	if !strings.Contains(w.out.String(), "WARN state unreadable") {
		t.Error("corrupt state should be a loud WARN")
	}
}

func TestRunWithoutStateFile(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	for i := 0; i < 2; i++ {
		if res := w.run(func(o *Options) { o.StatePath = "" }); res.ExitCode() != 0 {
			t.Fatal(res.Errors)
		}
		w.now = w.now.Add(time.Minute)
	}
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("stateless runs posted %d times, want 1", n)
	}
}

func TestRunSquattedChannel(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	w.cfg.Channels = append(w.cfg.Channels, Channel{Slug: "other", Title: "Other", MaxPerRun: 5, MaxAgeHours: 48,
		Feeds: []FeedConfig{{URL: w.feeds.url("/a")}}})
	// Someone else holds #news.
	squatter := testKey("squatter")
	sq := w.cfg.Channels[0]
	tok := clankmail.Sign(squatter, sq.manifest(squatter, "", testNow.UnixMilli()))
	if _, err := w.hub.srv.Client().Post(w.hub.srv.URL+"/v1/submit", "text/plain", strings.NewReader(tok)); err != nil {
		t.Fatal(err)
	}
	res := w.run()
	if res.ExitCode() == 0 {
		t.Fatal("a squatted channel must fail the run")
	}
	if !strings.Contains(w.out.String(), "is SQUATTED") {
		t.Error("squatting should be logged loudly")
	}
	if n := len(w.hub.posts("news")); n != 0 {
		t.Errorf("posted %d times into the squatted channel", n)
	}
	if n := len(w.hub.posts("other")); n != 1 {
		t.Errorf("the other channel got %d posts, want 1: one bad channel must not stop the rest", n)
	}
}

func TestRunManifestDriftUpdates(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run()
	cid := w.hub.slugs["news"]
	w.cfg.Channels[0].Title = "News, renamed"
	w.cfg.Channels[0].Tags = []string{"test", "more"}
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	man, err := clankmail.Verify(w.hub.manifest[cid])
	if err != nil {
		t.Fatal(err)
	}
	if man.Fields["title"] != "News, renamed" || man.Fields["channel"] != cid || len(strList(man.Fields["tags"])) != 2 {
		t.Fatalf("manifest after drift: %v", man.Fields)
	}
	w.now = w.now.Add(time.Minute)
	w.run()
	if n := w.hub.count("channel"); n != 2 {
		t.Errorf("%d channel objects, want genesis + one update (no update once in sync)", n)
	}
}

func TestRunHandle(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	w.cfg.Handle = "curator"
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if w.hub.count("handle") != 1 {
		t.Fatal("unbound handle was not claimed")
	}
	w.now = w.now.Add(time.Minute)
	w.run()
	if w.hub.count("handle") != 1 {
		t.Error("a handle already bound to us was claimed again")
	}

	// Held by another key: an error for that step, but channels still run.
	w2 := newWorld(t)
	w2.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w2.config(5, feed("/a"))
	w2.cfg.Handle = "curator"
	other := testKey("other")
	w2.hub.srv.Client().Post(w2.hub.srv.URL+"/v1/submit", "text/plain",
		strings.NewReader(clankmail.Sign(other, handlePayload(other, "curator", "hub.example", testNow.UnixMilli()))))
	res := w2.run()
	if res.ExitCode() == 0 || !strings.Contains(w2.out.String(), "held by another key") {
		t.Fatalf("handle held by another key: exit %d", res.ExitCode())
	}
	if n := len(w2.hub.posts("news")); n != 1 {
		t.Errorf("channel posts = %d, want 1 despite the handle error", n)
	}
}

func TestRunOneFeedFailing(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.feeds.set("/broken", "<rss><channel><item></rss>")
	w.config(5, feed("/a"), feed("/missing"), feed("/broken"))
	res := w.run()
	if res.ExitCode() != 0 {
		t.Fatalf("one good feed and two dead ones must not fail the run: %v", res.Errors)
	}
	if strings.Count(w.out.String(), "WARN channel=news feed ") != 2 {
		t.Error("each dead feed should be a WARN line")
	}
	if !strings.Contains(w.out.String(), "feeds_ok=1 feeds_failed=2") {
		t.Error("summary should count feeds")
	}
}

func TestRunAllFeedsFailing(t *testing.T) {
	w := newWorld(t)
	w.config(5, feed("/missing"), feed("/gone"))
	if res := w.run(); res.ExitCode() == 0 {
		t.Fatal("a run where no feed could be read must fail")
	}
}

func TestRunRateLimitStops(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(3)},
		entry{id: "2", link: "https://x.example/2", title: "2", date: ago(2)},
		entry{id: "3", link: "https://x.example/3", title: "3", date: ago(1)},
	))
	w.config(5, feed("/a"))
	w.cfg.Channels = append(w.cfg.Channels, Channel{Slug: "second", Title: "Second", MaxPerRun: 5, MaxAgeHours: 48,
		Feeds: []FeedConfig{{URL: w.feeds.url("/a")}}})
	w.hub.rateAfter = 2 // the genesis and one post
	res := w.run()
	if res.ExitCode() == 0 || !res.RateLimited {
		t.Fatalf("E_RATE must fail the run (exit %d, rate limited %v)", res.ExitCode(), res.RateLimited)
	}
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
	if _, ok := w.hub.slugs["second"]; ok {
		t.Error("submitting continued into the next channel after E_RATE")
	}
	if strings.Count(w.out.String(), "rate limited") != 1 {
		t.Error("submissions should stop at the first E_RATE")
	}
	// The state holds exactly what was posted, so the next run resumes.
	w.hub.rateAfter = 0
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1", "https://x.example/2", "https://x.example/3"}) {
		t.Fatalf("after E_RATE recovery: %v", got)
	}
}

func TestRunAmbiguousSubmitFailure(t *testing.T) {
	// The hub stores the post but the answer is lost (502). The curator must
	// find it stored and record it, or the next run would post it again.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A")) // first run: channel only
	w.config(5, feed("/a"))
	if res := w.run(); res.ExitCode() != 0 || len(w.hub.genesis) != 1 {
		t.Fatalf("channel not created: %v", res.Errors)
	}
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.hub.breakAfterStore = true
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatalf("a stored post answered with 502 should count as posted: %v", res.Errors)
	}
	w.now = w.now.Add(time.Minute)
	w.run()
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
}

func TestRunWrongIDFails(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.hub.wrongID = true
	if res := w.run(); res.ExitCode() == 0 {
		t.Fatal("a hub answering with the wrong id must fail the run")
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	res := w.run(func(o *Options) { o.DryRun = true })
	if res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if len(w.hub.submits) != 0 {
		t.Errorf("dry run submitted %v", w.hub.submits)
	}
	if _, err := os.Stat(w.statePath()); !os.IsNotExist(err) {
		t.Error("dry run wrote a state file")
	}
	if !strings.Contains(w.out.String(), `DRY-RUN channel=news would post "1" https://x.example/1`) {
		t.Error("dry run should print what it would post")
	}
}

func TestRunPersistsRebuiltState(t *testing.T) {
	// A run that rebuilds from the log but posts nothing still writes the
	// state, so the following run does not read the whole log again.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)}))
	w.config(5, feed("/a"))
	w.run()
	os.Remove(w.statePath())
	w.now = w.now.Add(time.Minute)
	w.run()
	st, err := LoadState(w.statePath())
	if err != nil || st == nil || st.Channels[w.hub.slugs["news"]] == nil {
		t.Fatalf("state after a no-post rebuild run: %v %v", st, err)
	}
	if _, ok := st.Channels[w.hub.slugs["news"]].Seen["https://x.example/1"]; !ok {
		t.Fatal("rebuilt seen set was not persisted")
	}
	w.now = w.now.Add(time.Minute)
	w.run()
	if strings.Contains(w.out.String(), "rebuilt seen set") {
		t.Error("third run rebuilt from the log again")
	}
}

func TestRunStateWriteFailure(t *testing.T) {
	// If the state cannot be written, submitting stops and the run fails.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(2)},
		entry{id: "2", link: "https://x.example/2", title: "2", date: ago(1)},
	))
	w.config(5, feed("/a"))
	bad := filepath.Join(w.dir, "missing-dir", "state.json")
	res := w.run(func(o *Options) { o.StatePath = bad })
	// The pending record is written before each submit, so an unwritable
	// state means nothing is posted at all.
	if res.ExitCode() == 0 || len(w.hub.posts("news")) != 0 {
		t.Fatalf("exit %d, %d posts: want failure before any post", res.ExitCode(), len(w.hub.posts("news")))
	}
}
