package curator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

// The xsearch and discord kinds are not in this build (they live in their
// own PR until each is verified against its real API): a config naming either
// must fail to load, not load with the source silently skipped.
func TestConfigRejectsXSearchAndDiscordKinds(t *testing.T) {
	for _, feedJSON := range []string{
		`{"kind":"xsearch","query":"MCP release"}`,
		`{"kind":"xsearch"}`,
		`{"kind":"xsearch","disabled":true}`,
		`{"kind":"discord","channel_id":"123456789012345678","guild_id":"123456789012345678"}`,
		`{"kind":"discord"}`,
		`{"kind":"discord","disabled":true}`,
	} {
		p := filepath.Join(t.TempDir(), "c.json")
		cfg := `{"hub":"hub.example","hub_url":"https://hub.example","channels":[{"slug":"news","title":"t","feeds":[` + feedJSON + `]}]}`
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("config with feed %s loaded", feedJSON)
		}
	}
	// The bare kind (no other fields) is rejected by the kind check itself,
	// not by the unknown-field check.
	for _, k := range []string{"xsearch", "discord"} {
		f := FeedConfig{Kind: k}
		if err := f.validate(); err == nil || !strings.Contains(err.Error(), `unknown kind "`+k+`"`) {
			t.Errorf("kind %s: %v", k, err)
		}
	}
	// Control: the same config with an rss feed loads.
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"hub":"hub.example","hub_url":"https://hub.example","channels":[{"slug":"news","title":"t","feeds":[{"url":"https://f.example"}]}]}`), 0o600)
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("control: %v", err)
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
