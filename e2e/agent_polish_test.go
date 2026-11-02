package e2e

// Onboarding polish: the first-digest hint, and the landing page's
// /api/latest endpoint on the mcp-http sidecar.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestClankFirstDigestHint(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	b := newAgent(t, h)
	a.ok("init")
	b.ok("init")
	b.ok("create", "quiet", "--title", "Quiet")
	a.ok("sub", "#quiet")
	if got := a.ok("digest"); !strings.HasPrefix(got, "no new posts (subscriptions start now; the first digest is usually empty)\nnext: --after ") {
		t.Fatalf("first digest after sub: %q", got)
	}
	// Only the first one: after that an empty digest is just empty.
	if got := a.ok("digest"); !strings.HasPrefix(got, "no new posts\nnext: --after ") {
		t.Fatalf("second digest: %q", got)
	}
}

type latestItem struct {
	Channel string  `json:"channel"`
	Slug    string  `json:"slug"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Link    *string `json:"link"`
}

func getLatest(t *testing.T, base, query string) (int, []latestItem, http.Header) {
	t.Helper()
	resp, err := http.Get(base + "/api/latest" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Items []latestItem }
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Items, resp.Header
}

func channelID(t *testing.T, created string) string {
	t.Helper()
	f := strings.Fields(created)
	if len(f) != 3 || !strings.HasPrefix(f[2], "c_") {
		t.Fatalf("create output: %q", created)
	}
	return f[2]
}

func TestClankAPILatest(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	news := channelID(t, a.ok("create", "news", "--title", "News"))
	plain := channelID(t, a.ok("create", "plain", "--title", "Plain"))
	evil := channelID(t, a.ok("create", "evil", "--title", "Evil"))
	secret := channelID(t, a.ok("create", "secret", "--title", "Secret", "--read", "members"))
	a.ok("post", "#news", "older")
	a.ok("post", "#news", "https://example.org/a\n\nmore words", "--subject", "Big news")
	a.ok("post", "#plain", "just text\nline two")
	a.ok("post", "#evil", "javascript:alert(1)\nsee http://ok.example/p for more", "--subject", "x‮y​")
	a.ok("post", "#secret", "members only")
	unknown := "c_" + strings.Repeat("a", 52)

	url := startHTTPMCP(t, a)
	base := strings.TrimSuffix(url, "/mcp")
	code, items, hdr := getLatest(t, base, "?c="+strings.Join([]string{news, plain, evil, secret, unknown}, ","))
	if code != 200 || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("latest: HTTP %d %v", code, hdr)
	}
	got := map[string]latestItem{}
	for _, it := range items {
		got[it.Channel] = it
	}
	link := func(it latestItem) string {
		if it.Link == nil {
			return "<nil>"
		}
		return *it.Link
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (members-only and unknown channels omitted), got %+v", items)
	}
	if it := got[news]; it.Title != "Big news" || link(it) != "https://example.org/a" || it.Slug != "news" || !strings.HasPrefix(it.ID, "m_") {
		t.Fatalf("news: %+v link %s", it, link(it))
	}
	if it := got[plain]; it.Title != "just text" || it.Link != nil {
		t.Fatalf("plain: %+v link %s", it, link(it))
	}
	if it := got[evil]; it.Title != "xy" || link(it) != "http://ok.example/p" {
		t.Fatalf("evil (cleaned title, no javascript: link): %+v link %s", it, link(it))
	}

	// Validation: 1-8 c_ ids in one c parameter, nothing else; GET only.
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = fmt.Sprintf("c_%s%c", strings.Repeat("b", 51), 'a'+rune(i))
	}
	for _, q := range []string{"", "?c=", "?c=" + strings.Join(nine, ","), "?c=c_bad", "?c=%23news", "?c=" + news + "&c=" + plain, "?c=" + news + "&x=1", "?c=" + news + ",https://evil.example/"} {
		if code, _, _ := getLatest(t, base, q); code != 400 {
			t.Fatalf("latest%s: HTTP %d, want 400", q, code)
		}
	}
	// Its positive control: eight ids are accepted.
	if code, _, _ := getLatest(t, base, "?c="+strings.Join(nine[:8], ",")); code != 200 {
		t.Fatalf("eight ids: HTTP %d, want 200", code)
	}
	if resp, err := http.Post(base+"/api/latest?c="+news, "text/plain", strings.NewReader("x")); err != nil || resp.StatusCode != 405 {
		t.Fatalf("POST /api/latest: %v %v", resp.StatusCode, err)
	}

	// Cached: a newer post does not show within the TTL...
	first := got[news].ID
	a.ok("post", "#news", "https://example.org/b", "--subject", "Newer news")
	if _, items, _ := getLatest(t, base, "?c="+news); len(items) != 1 || items[0].ID != first {
		t.Fatalf("within the cache TTL: %+v, want the cached %s", items, first)
	}
	// ...while a server without the cache shows it.
	fresh := &agent{t: t, env: append(os.Environ(), "CLANK_HUB="+h.URL, "CLANK_HUB_NAME="+hubName, "CLANK_KEY="+a.key, "CLANK_LATEST_TTL_MS=1")}
	base2 := strings.TrimSuffix(startHTTPMCP(t, fresh), "/mcp")
	if _, items, _ := getLatest(t, base2, "?c="+news); len(items) != 1 || items[0].Title != "Newer news" {
		t.Fatalf("uncached: %+v, want Newer news", items)
	}
}

// TestClankAPILatestLongLog: the newest post of a log far past the scan cap.
func TestClankAPILatestLongLog(t *testing.T) {
	bh := startBigHub(t, 5100, 0)
	a := bigAgent(t, bh)
	base := strings.TrimSuffix(startHTTPMCP(t, a), "/mcp")
	if code, items, _ := getLatest(t, base, "?c="+bh.channel); code != 200 || len(items) != 1 || items[0].Title != "post 5100" {
		t.Fatalf("latest of 5100 posts: HTTP %d %+v", code, items)
	}
}
