package curator

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectPerFeedCap(t *testing.T) {
	busy := &Feed{Title: "Busy", Items: []Item{
		{Link: "https://busy.example/1", Title: "b1", Date: at(1)},
		{Link: "https://busy.example/2", Title: "b2", Date: at(2)},
		{Link: "https://busy.example/3", Title: "b3", Date: at(3)},
	}}
	quiet := &Feed{Title: "Quiet", Items: []Item{{Link: "https://quiet.example/1", Title: "q1", Date: at(10)}}}
	ch := Channel{MaxPerRun: 2, Feeds: []FeedConfig{{MaxPerRun: 1}, {}}}
	got, _ := sel(t, ch, nil, busy, quiet)
	if want := []string{"https://quiet.example/1", "https://busy.example/1"}; !reflect.DeepEqual(lines(got), want) {
		t.Fatalf("selected %v, want %v: the busy feed must take only its newest one", lines(got), want)
	}
	ch.Feeds[0].MaxPerRun = 0 // no per-feed cap: unchanged behaviour
	got, _ = sel(t, ch, nil, busy, quiet)
	if want := []string{"https://busy.example/2", "https://busy.example/1"}; !reflect.DeepEqual(lines(got), want) {
		t.Fatalf("selected %v, want %v", lines(got), want)
	}
}

// bskyRSS is the shape of https://bsky.app/profile/<handle>/rss (read
// 2026-11-08): items with no <title>, dates without a weekday or seconds.
const bskyRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><description>about</description><link>https://bsky.app/profile/a.example</link><title>@a.example - A</title>` +
	`<item><link>https://bsky.app/profile/a.example/post/3abc</link><description>Shipped an MCP server for &#39;datasette&#39; today: https://example.com/x</description>` +
	`<pubDate>01 Nov 2026 09:30 +0000</pubDate><guid isPermaLink="false">at://did:plc:x/app.bsky.feed.post/3abc</guid></item></channel></rss>`

func TestBlueskyFeedTitleFromSummary(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/bsky", bskyRSS)
	f := feed("/bsky")
	f.TitleFromSummary = true
	w.config(5, f)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatalf("exit %d %v", res.ExitCode(), res.Errors)
	}
	p := w.hub.posts("news")
	if len(p) != 1 || p[0].Fields["subject"] != "Shipped an MCP server for 'datasette' today: https://example.com/x" ||
		p[0].Fields["body"] != "https://bsky.app/profile/a.example/post/3abc\n\nShipped an MCP server for 'datasette' today: https://example.com/x\n\nvia @a.example - A" {
		t.Fatalf("posts %v", p)
	}
	// Without the option an untitled item keeps the host as its subject.
	w2 := newWorld(t)
	w2.feeds.set("/bsky", bskyRSS)
	w2.config(5, feed("/bsky"))
	w2.run()
	if s := w2.hub.posts("news")[0].Fields["subject"]; s != "bsky.app" {
		t.Errorf("subject %q, want the host fallback", s)
	}
}

func TestDisabledSourceNotFetched(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "one", date: ago(1)}))
	w.feeds.set("/b", atomFeedXML("B", entry{id: "2", link: "https://x.example/2", title: "two", date: ago(1)}))
	b := feed("/b")
	b.Disabled = true
	w.config(5, feed("/a"), b)
	res := w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) || res.FeedsOK != 1 || res.FeedsFailed != 0 {
		t.Fatalf("posted %v ok %d failed %d", got, res.FeedsOK, res.FeedsFailed)
	}
}

func TestNormalizeLinkMapsTwitterToX(t *testing.T) {
	for in, want := range map[string]string{
		"https://twitter.com/a/status/1":        "https://x.com/a/status/1",
		"https://Mobile.Twitter.com/a/status/1": "https://x.com/a/status/1",
		"https://www.x.com/a/status/1?utm_x=1":  "https://x.com/a/status/1",
		"https://x.com/a/status/1":              "https://x.com/a/status/1",
		"https://nottwitter.com/a":              "https://nottwitter.com/a",
		"https://twitter.com.example/a":         "https://twitter.com.example/a",
		"https://twitter.com:8443/a":            "https://twitter.com:8443/a",
	} {
		if got := NormalizeLink(in); got != want {
			t.Errorf("NormalizeLink(%q) = %q, want %q", in, got, want)
		}
		if got := NormalizeLink(want); got != want {
			t.Errorf("not idempotent on %q: %q", want, got)
		}
	}
}

// A state file written before twitter.com mapped to x.com still dedupes the
// same item: its keys are re-derived on load.
func TestStateRekeysTwitterLinks(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://twitter.com/a/status/1", title: "tweet", date: ago(1)}))
	w.config(5, feed("/a"))
	w.run() // creates the channel; posts the x.com form
	id := ""
	for k := range mustState(t, w).Channels {
		id = k
	}
	if id == "" || len(w.hub.posts("news")) != 1 {
		t.Fatalf("setup: %d posts", len(w.hub.posts("news")))
	}
	// Rewrite the state as an old binary would have: the twitter.com key.
	old := `{"version":1,"channels":{"` + id + `":{"slug":"news","seen":{"https://twitter.com/a/status/1":1}}}}`
	if err := os.WriteFile(w.statePath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// A later clock: a repost would sign different bytes (same-ts bytes are
	// the same object, which the hub would just confirm).
	w.now = w.now.Add(time.Hour)
	w.run()
	if n := len(w.hub.posts("news")); n != 1 {
		t.Fatalf("%d posts: an old twitter.com seen key did not match the x.com item", n)
	}
	if !strings.Contains(w.out.String(), "seen=1") {
		t.Errorf("want seen=1 in the summary")
	}
}

func mustState(t *testing.T, w *world) *State {
	t.Helper()
	s, err := LoadState(w.statePath())
	if err != nil || s == nil {
		t.Fatalf("state: %v", err)
	}
	return s
}
