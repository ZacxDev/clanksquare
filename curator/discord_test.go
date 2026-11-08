package curator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testGuild   = "111111111111111111"
	testChannel = "222222222222222222"
	testBotTok  = "bot-TESTTOKEN-not-real-abcdef"
)

// did is a Discord message id created h hours before testNow, plus n ms.
func did(h float64, n int64) string {
	ms := at(h).UnixMilli() - discordEpochMS + n
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

type dmsg struct {
	id, content, author string
	embeds              []map[string]string
	typ                 int
}

type fakeDiscord struct {
	srv    *httptest.Server
	mu     sync.Mutex
	msgs   []dmsg
	afters []string
	auths  []string
	// queued responses served before the normal one: status, headers, body.
	queue []struct {
		status int
		hdr    map[string]string
		body   string
	}
	hdr map[string]string // headers on normal responses
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	d := &fakeDiscord{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.URL.Path != "/api/v10/channels/"+testChannel+"/messages" {
			http.NotFound(w, r)
			return
		}
		d.afters = append(d.afters, r.URL.Query().Get("after"))
		d.auths = append(d.auths, r.Header.Get("Authorization"))
		if len(d.queue) > 0 {
			q := d.queue[0]
			d.queue = d.queue[1:]
			for k, v := range q.hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(q.status)
			io.WriteString(w, q.body)
			return
		}
		after := r.URL.Query().Get("after")
		// The (at most) 100 messages right after `after`, newest first.
		var page []dmsg
		for _, m := range d.msgs { // d.msgs is oldest first
			if snowflakeLess(after, m.id) && len(page) < 100 {
				page = append(page, m)
			}
		}
		var out []map[string]any
		for i := len(page) - 1; i >= 0; i-- {
			m := page[i]
			ts := snowflakeTime(m.id, discordEpochMS).Format(time.RFC3339Nano)
			out = append(out, map[string]any{"id": m.id, "type": m.typ, "content": m.content, "timestamp": ts,
				"author": map[string]any{"username": m.author, "bot": true}, "embeds": m.embeds})
		}
		if out == nil {
			out = []map[string]any{}
		}
		for k, v := range d.hdr {
			w.Header().Set(k, v)
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func dworld(t *testing.T) (*world, *fakeDiscord, func(*Options), *[]time.Duration) {
	w := newWorld(t)
	d := newFakeDiscord(t)
	c := &Config{Hub: "hub.example", HubURL: w.hub.srv.URL, Channels: []Channel{{
		Slug: "news", Title: "News", About: "test channel", Tags: []string{"test"}, MaxPerRun: 10,
		Feeds: []FeedConfig{{Kind: KindDiscord, ChannelID: testChannel, GuildID: testGuild}},
	}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	w.cfg = c
	tok := filepath.Join(w.dir, "discord.token")
	if err := os.WriteFile(tok, []byte(testBotTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	var mu sync.Mutex
	return w, d, func(o *Options) {
		o.DiscordTokenFile = tok
		o.discordBase = d.srv.URL
		o.sleep = func(_ context.Context, dur time.Duration) error {
			mu.Lock()
			slept = append(slept, dur)
			mu.Unlock()
			return nil
		}
	}, &slept
}

func TestDiscordPostsAndPersistsCursor(t *testing.T) {
	w, d, opt, _ := dworld(t)
	m1, m2, m3 := did(3, 0), did(2, 0), did(1, 0)
	d.msgs = []dmsg{
		{id: m1, author: "Ollama #announcements", content: "Ollama 0.13 is out!\nFaster loads. https://ollama.com/blog/0.13."},
		{id: m2, author: "MCP #news", content: "", embeds: []map[string]string{{"title": "Spec 2026-11", "description": "New auth flow", "url": "https://modelcontextprotocol.io/spec?utm_source=discord"}}},
		{id: m3, author: "Some Server #general", content: "no link in this one\nsecond line"},
		{id: did(0.5, 0), author: "x", content: "joined", typ: 7}, // a join notice: never an item
	}
	res := w.run(opt)
	if res.ExitCode() != 0 {
		t.Fatalf("exit %d: %v", res.ExitCode(), res.Errors)
	}
	permalink := "https://discord.com/channels/" + testGuild + "/" + testChannel + "/" + m3
	want := []string{"https://ollama.com/blog/0.13", "https://modelcontextprotocol.io/spec", permalink}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, want) {
		t.Fatalf("posted %v\nwant %v", got, want)
	}
	p := w.hub.posts("news")
	if p[0].Fields["subject"] != "Ollama 0.13 is out!" || p[0].Fields["body"] != "https://ollama.com/blog/0.13\n\nFaster loads. https://ollama.com/blog/0.13.\n\nvia Ollama #announcements (Discord)" {
		t.Errorf("post 1: %q / %q", p[0].Fields["subject"], p[0].Fields["body"])
	}
	if p[1].Fields["subject"] != "Spec 2026-11" || !strings.Contains(p[1].Fields["body"].(string), "New auth flow") {
		t.Errorf("post 2 (embed only): %q / %q", p[1].Fields["subject"], p[1].Fields["body"])
	}
	if d.auths[0] != "Bot "+testBotTok || strings.Contains(w.out.String(), testBotTok) {
		t.Errorf("auth %q, or the token leaked into the log", d.auths[0])
	}
	// First run, no cursor: start at max_age_hours (48) ago.
	if d.afters[0] != discordSnowflakeAt(testNow.Add(-48*time.Hour)) {
		t.Errorf("first after=%s", d.afters[0])
	}
	st, err := LoadState(w.statePath())
	if err != nil || st.DiscordCursors[testChannel] != did(0.5, 0) {
		t.Fatalf("cursor %v (%v), want the newest id read %s", st.DiscordCursors, err, did(0.5, 0))
	}
	// Run 2 asks only for what came after the cursor.
	m4 := did(0.25, 0)
	d.msgs = append(d.msgs, dmsg{id: m4, author: "Ollama #announcements", content: "0.13.1 https://ollama.com/blog/0.13.1"})
	w.run(opt)
	if got := d.afters[len(d.afters)-1]; got != did(0.5, 0) {
		t.Fatalf("run 2 asked after=%s, want the persisted cursor %s", got, did(0.5, 0))
	}
	if n := len(w.hub.posts("news")); n != 4 {
		t.Errorf("%d posts after run 2, want 4", n)
	}
}

func TestDiscordPaginates(t *testing.T) {
	w, d, opt, _ := dworld(t)
	for i := 0; i < 150; i++ {
		d.msgs = append(d.msgs, dmsg{id: did(10, int64(i)), author: "S", content: fmt.Sprintf("m%d https://e.example/%d", i, i)})
	}
	w.run(opt)
	// Page 1 returns the 100 newest after the start; page 2 asks after its newest.
	if len(d.afters) != 2 {
		t.Fatalf("requests %d: %v", len(d.afters), d.afters)
	}
	st, _ := LoadState(w.statePath())
	if st.DiscordCursors[testChannel] != did(10, 149) {
		t.Errorf("cursor %s, want the newest id %s", st.DiscordCursors[testChannel], did(10, 149))
	}
}

func TestDiscord429RetryAfter(t *testing.T) {
	w, d, opt, slept := dworld(t)
	d.msgs = []dmsg{{id: did(1, 0), author: "S", content: "hi https://e.example/1"}}
	d.queue = append(d.queue, struct {
		status int
		hdr    map[string]string
		body   string
	}{429, map[string]string{"Retry-After": "9"}, `{"message":"You are being rate limited.","retry_after":1.5,"global":false}`})
	res := w.run(opt)
	if res.ExitCode() != 0 || len(w.hub.posts("news")) != 1 {
		t.Fatalf("exit %d posts %d", res.ExitCode(), len(w.hub.posts("news")))
	}
	if !reflect.DeepEqual(*slept, []time.Duration{1500 * time.Millisecond}) {
		t.Errorf("slept %v, want the body's retry_after 1.5s", *slept)
	}
}

func TestDiscord429TooLongFailsFeed(t *testing.T) {
	w, d, opt, slept := dworld(t)
	d.queue = append(d.queue, struct {
		status int
		hdr    map[string]string
		body   string
	}{429, nil, `{"retry_after":3600}`})
	res := w.run(opt)
	if res.FeedsFailed != 1 || len(*slept) != 0 {
		t.Fatalf("failed %d slept %v: a wait over the bound must fail the feed, not sleep", res.FeedsFailed, *slept)
	}
	if st, _ := LoadState(w.statePath()); st.DiscordCursors[testChannel] != "" {
		t.Errorf("a failed read moved the cursor to %s", st.DiscordCursors[testChannel])
	}
}

func TestDiscordBucketExhaustedWaits(t *testing.T) {
	w, d, opt, slept := dworld(t)
	d.msgs = []dmsg{{id: did(1, 0), author: "S", content: "hi https://e.example/1"}}
	d.hdr = map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "2.25"}
	w.run(opt)
	if !reflect.DeepEqual(*slept, []time.Duration{2250 * time.Millisecond}) {
		t.Errorf("slept %v, want X-RateLimit-Reset-After 2.25s", *slept)
	}
}

func TestDiscordConfigValidation(t *testing.T) {
	for i, f := range []FeedConfig{
		{Kind: KindDiscord, ChannelID: testChannel},
		{Kind: KindDiscord, ChannelID: "general", GuildID: testGuild},
		{Kind: KindDiscord, ChannelID: testChannel, GuildID: testGuild, URL: "https://x.example"},
		{Kind: KindRSS, URL: "https://x.example", ChannelID: testChannel},
	} {
		if err := f.validate(); err == nil {
			t.Errorf("bad[%d] %+v validated", i, f)
		}
	}
}
