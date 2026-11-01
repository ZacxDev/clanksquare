package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clankmail/go/curator"
)

// TestCuratorEndToEnd runs the feed curator (go/curator) against this real
// hub and a local feed server, on the real clock. It is the end-to-end check
// that state is only a cache: deleting it must not repost anything.
func TestCuratorEndToEnd(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := New(Config{Name: hubName, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	hubSrv := httptest.NewServer(h)
	t.Cleanup(hubSrv.Close)

	var mu sync.Mutex
	feeds := map[string]string{}
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body, ok := feeds[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(feedSrv.Close)
	setFeed := func(path, body string) { mu.Lock(); feeds[path] = body; mu.Unlock() }

	now := time.Now().UTC()
	date := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(time.RFC1123Z) }
	rss := func(items ...[3]string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Local feed</title>`)
		for _, it := range items {
			fmt.Fprintf(&b, "<item><title>%s</title><link>%s</link><pubDate>%s</pubDate><description>&lt;p&gt;about %s&lt;/p&gt;</description></item>", it[0], it[1], it[2], it[0])
		}
		b.WriteString("</channel></rss>")
		return b.String()
	}
	setFeed("/a.xml", rss(
		[3]string{"one", "https://example.org/1?utm_source=feed", date(3)},
		[3]string{"two", "https://example.org/2", date(2)},
	))
	// The second channel's feeds overlap: the shared link posts once there.
	setFeed("/b.xml", rss([3]string{"shared", "https://example.org/shared", date(1)}))
	setFeed("/c.xml", rss([3]string{"shared again", "https://example.org/shared#x", date(1)}))

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "curator.key")
	kid, err := curator.GenerateKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := curator.LoadKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &curator.Config{Hub: hubName, HubURL: hubSrv.URL, Handle: "curator", Channels: []curator.Channel{
		{Slug: "alpha", Title: "Alpha", About: "e2e", Tags: []string{"e2e"}, Feeds: []curator.FeedConfig{{URL: feedSrv.URL + "/a.xml"}}},
		{Slug: "beta", Title: "Beta", Feeds: []curator.FeedConfig{{URL: feedSrv.URL + "/b.xml"}, {URL: feedSrv.URL + "/c.xml"}, {URL: feedSrv.URL + "/dead.xml"}}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	run := func(label string) {
		t.Helper()
		var out bytes.Buffer
		res := curator.Run(context.Background(), curator.Options{Config: cfg, Key: key, StatePath: statePath, Out: &out})
		t.Logf("%s:\n%s", label, out.String())
		if res.ExitCode() != 0 {
			t.Fatalf("%s: exit %d: %v", label, res.ExitCode(), res.Errors)
		}
	}
	// posts returns the first body lines of the curator's posts in a channel,
	// read back from the hub's own log and verified.
	posts := func(slug string) []string {
		t.Helper()
		var ch struct{ Channel string }
		getJSON(t, hubSrv.URL+"/v1/channels/%23"+slug, &ch)
		var out []string
		after := ""
		for {
			var page struct {
				Items []struct{ Token string }
				Next  string
			}
			getJSON(t, hubSrv.URL+"/v1/channels/"+ch.Channel+"/log?after="+after, &page)
			for _, it := range page.Items {
				obj, err := clankmail.Verify(it.Token)
				if err != nil || obj.From != kid {
					t.Fatalf("log holds a token not signed by the curator: %v", err)
				}
				line, _, _ := strings.Cut(obj.Fields["body"].(string), "\n")
				out = append(out, line)
			}
			if len(page.Items) == 0 {
				return out
			}
			after = page.Next
		}
	}
	check := func(label, slug string, want ...string) {
		t.Helper()
		if got := posts(slug); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s: #%s holds %v, want %v", label, slug, got, want)
		}
	}

	run("run 1")
	check("run 1", "alpha", "https://example.org/1", "https://example.org/2")
	check("run 1", "beta", "https://example.org/shared")
	var hd struct{ Handle string }
	getJSON(t, hubSrv.URL+"/v1/handles/curator", &hd)
	if obj, err := clankmail.Verify(hd.Handle); err != nil || obj.From != kid {
		t.Fatalf("handle @curator not bound to the curator key: %v", err)
	}

	run("run 2")
	check("run 2", "alpha", "https://example.org/1", "https://example.org/2")
	check("run 2", "beta", "https://example.org/shared")

	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	run("run 3 (state deleted)")
	check("run 3", "alpha", "https://example.org/1", "https://example.org/2")
	check("run 3", "beta", "https://example.org/shared")

	setFeed("/a.xml", rss(
		[3]string{"one", "https://example.org/1?utm_source=feed", date(3)},
		[3]string{"two", "https://example.org/2", date(2)},
		[3]string{"three", "https://example.org/3", date(0)},
	))
	run("run 4 (new item)")
	check("run 4", "alpha", "https://example.org/1", "https://example.org/2", "https://example.org/3")
	run("run 5")
	check("run 5", "alpha", "https://example.org/1", "https://example.org/2", "https://example.org/3")
	check("run 5", "beta", "https://example.org/shared")
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
