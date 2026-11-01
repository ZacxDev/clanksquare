package curator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func parseFixture(t *testing.T, name string) *Feed {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseFeed(raw)
	if err != nil {
		t.Fatalf("ParseFeed(%s): %v", name, err)
	}
	return f
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestParseRSS2(t *testing.T) {
	f := parseFixture(t, "rss2.xml")
	if f.Title != "Example & News" {
		t.Errorf("title = %q", f.Title)
	}
	want := []Item{
		{GUID: "post-1", Link: "https://Blog.Example/post/1?utm_source=hn&id=7&UTM_Medium=rss#comments",
			Title: "Show HN: A self-hosted thing", Summary: "First para & more. Second link",
			Date: utc("2026-10-31T12:00:00Z")},
		{GUID: "https://blog.example/post/2", Title: "Escaped markup title", Summary: "Escaped & HTML",
			Date: utc("2026-10-31T12:00:00Z")}, // 07:00 EST
		{Link: "https://blog.example/post/3", Title: "Undated item"},
		{Link: "https://blog.example/post/4", Title: "Content only", Summary: "From content:encoded",
			Date: utc("2026-10-30T09:30:00Z")},
	}
	checkItems(t, f.Items, want)
}

func TestParseRDF(t *testing.T) {
	f := parseFixture(t, "rdf.xml")
	checkItems(t, f.Items, []Item{{GUID: "https://rdf.example/a", Link: "https://rdf.example/a", Title: "RDF item",
		Summary: "Plain description", Date: utc("2026-10-31T08:00:00Z")}})
}

func TestParseGitHubReleases(t *testing.T) {
	f := parseFixture(t, "github-releases.atom")
	if f.Title != "Release notes from ollama" {
		t.Errorf("title = %q", f.Title)
	}
	checkItems(t, f.Items, []Item{
		{GUID: "tag:github.com,2008:Repository/658928958/v0.40.13",
			Link:    "https://github.com/ollama/ollama/releases/tag/v0.40.13",
			Title:   "v0.40.13",
			Summary: "LFM3 on Linux and Windows Liquid AI's LFM3 models now run on Linux. Full Changelog : v0.40.12...v0.40.13",
			Date:    utc("2026-10-29T02:54:11Z")}, // no <published>: falls back to <updated>
		{GUID: "tag:github.com,2008:Grit::Commit/c65f52f77152526cc27554532cd316616ec8ce61",
			Link:    "https://github.com/ZacxDev/clankmail/commit/c65f52f77152526cc27554532cd316616ec8ce61",
			Title:   "Merge pull request #8 from ZacxDev/feat/sub-digest",
			Summary: "Merge pull request #8 from ZacxDev/feat/sub-digest Subscriptions (sub, §7.4)",
			Date:    utc("2026-11-01T08:06:49Z")},
	})
}

func TestParseArxiv(t *testing.T) {
	f := parseFixture(t, "arxiv.atom")
	checkItems(t, f.Items, []Item{
		{GUID: "http://arxiv.org/abs/2610.40641v1", Link: "https://arxiv.org/abs/2610.40641v1",
			Title:   "From extreme weather to agricultural consequences with agentic AI",
			Summary: "Extreme weather is forecast days ahead, yet its consequences for agriculture remain hard to predict: x < y.",
			Date:    utc("2026-10-29T17:58:53Z")},
		{GUID: "http://arxiv.org/abs/2610.40000v2", Link: "https://arxiv.org/abs/2610.40000v2",
			Title: "An xhtml title", Summary: "Reagents, not tools.",
			Date: utc("2026-10-28T01:00:00Z")}, // published wins over updated
	})
}

func checkItems(t *testing.T, got, want []Item) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.GUID != w.GUID || g.Link != w.Link || g.Title != w.Title || g.Summary != w.Summary || !g.Date.Equal(w.Date) {
			t.Errorf("item %d:\n got  %+v\n want %+v", i, g, w)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, name := range []string{"malformed.xml", "notafeed.html"} {
		raw, _ := os.ReadFile("testdata/" + name)
		if _, err := ParseFeed(raw); err == nil {
			t.Errorf("ParseFeed(%s) succeeded, want an error", name)
		}
	}
}

func TestParseDateIsHostIndependent(t *testing.T) {
	// "EST" must not be resolved against the local zone, and an unknown zone
	// name is undated rather than silently UTC.
	if got := parseDate("Sat, 31 Oct 2026 07:00:00 EST"); !got.Equal(utc("2026-10-31T12:00:00Z")) {
		t.Errorf("EST parsed as %v", got)
	}
	if got := parseDate("Sat, 31 Oct 2026 07:00:00 XYZ"); !got.IsZero() {
		t.Errorf("unknown zone parsed as %v, want undated", got)
	}
	if got := parseDate("yesterday"); !got.IsZero() {
		t.Errorf("garbage parsed as %v", got)
	}
}

func TestFetchFeedLimits(t *testing.T) {
	rss, _ := os.ReadFile("testdata/rss2.xml")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if ua := r.Header.Get("User-Agent"); ua != UserAgent {
				http.Error(w, "bad UA "+ua, 400)
				return
			}
			w.Write(rss)
		case "/big":
			// A valid feed start followed by more than MaxFeedBytes.
			w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>big</title><description>`))
			chunk := []byte(strings.Repeat("x", 1<<16))
			for n := 0; n <= MaxFeedBytes; n += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if f, err := FetchFeed(ctx, srv.Client(), srv.URL+"/ok"); err != nil || len(f.Items) != 4 {
		t.Fatalf("ok feed: %v, %v", f, err)
	}
	if _, err := FetchFeed(ctx, srv.Client(), srv.URL+"/big"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized feed: err = %v, want an exceeds error", err)
	}
	if _, err := FetchFeed(ctx, srv.Client(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404 feed: err = %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := FetchFeed(short, srv.Client(), srv.URL+"/slow"); err == nil {
		t.Error("slow feed: want a timeout error")
	}
}
