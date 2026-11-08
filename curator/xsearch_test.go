package curator

import (
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

// xid is an X status id created h hours before testNow.
func xid(h float64) string {
	ms := at(h).UnixMilli() - twitterEpochMS
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

type fakeXAI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []map[string]any
	auths  []string
	status int
	body   string
}

func newFakeXAI(t *testing.T) *fakeXAI {
	x := &fakeXAI{status: 200}
	x.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(raw, &m)
		x.mu.Lock()
		x.reqs = append(x.reqs, m)
		x.auths = append(x.auths, r.Header.Get("Authorization"))
		st, b := x.status, x.body
		x.mu.Unlock()
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(st)
		io.WriteString(w, b)
	}))
	t.Cleanup(x.srv.Close)
	return x
}

func (x *fakeXAI) calls() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.reqs)
}

// xaiBody builds a Responses API body: output text, citations and usage.
func xaiBody(text string, citations []string, usage string) string {
	t, _ := json.Marshal(text)
	c, _ := json.Marshal(citations)
	if usage == "" {
		usage = `{"input_tokens":30000,"output_tokens":2000,"input_tokens_details":{"cached_tokens":10000},
		  "server_side_tool_usage_details":{"x_search_calls":2,"x_posts_fetched":20,"x_users_fetched":1}}`
	}
	return fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%s,"annotations":[]}]}],"citations":%s,"usage":%s}`, t, c, usage)
}

const testXAIKey = "xai-TESTKEY-not-real-0123456789"

// xworld is a world with one rss feed and one xsearch source in one channel.
func xworld(t *testing.T, fc FeedConfig) (*world, *fakeXAI, func(*Options)) {
	w := newWorld(t)
	x := newFakeXAI(t)
	w.feeds.set("/a", atomFeedXML("Feed A", entry{id: "1", link: "https://rss.example/1", title: "RSS one", date: ago(1)}))
	fc.Kind = KindXSearch
	c := &Config{Hub: "hub.example", HubURL: w.hub.srv.URL, Channels: []Channel{{
		Slug: "news", Title: "News", About: "test channel", Tags: []string{"test"}, MaxPerRun: 10,
		Feeds: []FeedConfig{{URL: w.feeds.url("/a")}, fc},
	}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	w.cfg = c
	keyFile := filepath.Join(w.dir, "xai.key")
	if err := os.WriteFile(keyFile, []byte(testXAIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return w, x, func(o *Options) { o.XAIKeyFile = keyFile; o.xaiBase = x.srv.URL }
}

func readLedger(t *testing.T, w *world) *spendLedger {
	t.Helper()
	l, err := loadLedger(ledgerPath(w.statePath()))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func writeLedger(t *testing.T, w *world, l spendLedger) {
	t.Helper()
	if l.LastCallMS == nil {
		l.LastCallMS = map[string]int64{}
	}
	if err := l.save(ledgerPath(w.statePath())); err != nil {
		t.Fatal(err)
	}
}

func TestXSearchPostsCitedStatuses(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{AllowedXHandles: []string{"alice", "bob"}})
	a, b, i, old, stranger, invented := xid(2), xid(3), xid(4), xid(30), xid(1), xid(1.5)
	x.body = xaiBody("Here you go:\n```json\n"+`[
	  {"url":"https://x.com/alice/status/`+a+`","handle":"alice","text":"Alice ships MCP 2.0\nmore"},
	  {"url":"https://x.com/i/status/`+i+`","handle":"bob","text":"Bob via an i-link"},
	  {"url":"https://x.com/alice/status/`+invented+`","handle":"alice","text":"never cited"}
	]`+"\n```",
		[]string{
			"https://twitter.com/alice/status/" + a + "?s=20",
			"https://x.com/Bob/status/" + b,
			"https://x.com/i/status/" + i,
			"https://x.com/alice/status/" + old,        // older than lookback_hours
			"https://x.com/mallory/status/" + stranger, // not an allowed handle
			"https://x.com/i/user/1912644073896206336", // a profile, not a post
			"https://example.com/article",
		}, "")
	res := w.run(opt)
	if res.ExitCode() != 0 {
		t.Fatalf("exit %d: %v", res.ExitCode(), res.Errors)
	}
	got := w.hub.firstLines("news")
	want := []string{"https://x.com/i/status/" + i, "https://x.com/i/status/" + b, "https://x.com/i/status/" + a, "https://rss.example/1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("posted %v\nwant %v", got, want)
	}
	byLine := map[string]map[string]any{}
	for _, p := range w.hub.posts("news") {
		body := p.Fields["body"].(string)
		line, _, _ := strings.Cut(body, "\n")
		byLine[line] = p.Fields
	}
	pa := byLine["https://x.com/i/status/"+a]
	if pa["subject"] != "Alice ships MCP 2.0 more" || pa["body"] != "https://x.com/i/status/"+a+"\n\nAlice ships MCP 2.0 more\n\nvia @alice on X (found by xAI x_search)" {
		t.Errorf("alice post: %q / %q", pa["subject"], pa["body"])
	}
	if pi := byLine["https://x.com/i/status/"+i]; !strings.HasSuffix(pi["body"].(string), "via @bob on X (found by xAI x_search)") {
		t.Errorf("i-link post should take the reported, allowed handle: %q", pi["body"])
	}
	if pb := byLine["https://x.com/i/status/"+b]; pb["subject"] != "Post by @Bob on X" {
		t.Errorf("uncommented post subject %q", pb["subject"])
	}
	// The request: the key from the file as a bearer token, the handles, the window.
	if x.calls() != 1 || x.auths[0] != "Bearer "+testXAIKey {
		t.Fatalf("calls %d auth %q", x.calls(), x.auths)
	}
	tool := x.reqs[0]["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "x_search" || !reflect.DeepEqual(tool["allowed_x_handles"], []any{"alice", "bob"}) || tool["from_date"] != "2026-10-31" {
		t.Errorf("tool %v", tool)
	}
	if x.reqs[0]["model"] != "grok-4.7" || x.reqs[0]["max_turns"] != float64(2) {
		t.Errorf("request %v", x.reqs[0])
	}
	if strings.Contains(w.out.String(), testXAIKey) {
		t.Errorf("the API key appears in the log")
	}
	// Settled to the reported usage: 20 posts $0.10 + 1 profile $0.01 +
	// 20k input $0.04 + 10k cached $0.005 + 2k output $0.012 = $0.167.
	if l := readLedger(t, w); l.MicroUSD != 167_000 || l.Calls != 1 || l.Month != "2026-11" {
		t.Errorf("ledger %+v, want 167000 µ$ over 1 call in 2026-11", l)
	}
}

// The worst case of one default call, pinned: 300 posts $1.50 + 40 profiles
// $0.40 + 240k input at $4/M $0.96 + 6k output at $12/M $0.072.
const testWorst = 2_932_000

func TestXSearchWorstCaseIsPinned(t *testing.T) {
	fc := FeedConfig{Kind: KindXSearch, Query: "q"}
	if err := fc.validate(); err != nil {
		t.Fatal(err)
	}
	if got := worstCaseMicro(fc); got != testWorst {
		t.Fatalf("worst case %d µ$, want %d", got, testWorst)
	}
}

func TestXSearchSpendCapAtBoundaryCalls(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	capMicro := int64(60_000_000)
	writeLedger(t, w, spendLedger{Version: 1, Month: "2026-11", MicroUSD: capMicro - testWorst, Calls: 7})
	x.body = xaiBody("[]", []string{"https://x.com/alice/status/" + xid(1)}, "")
	res := w.run(opt)
	if x.calls() != 1 {
		t.Fatalf("estimate + worst case == cap must still call: %d calls\n%s", x.calls(), w.out.String())
	}
	if res.ExitCode() != 0 || len(w.hub.posts("news")) != 2 {
		t.Fatalf("exit %d, %d posts", res.ExitCode(), len(w.hub.posts("news")))
	}
	if l := readLedger(t, w); l.MicroUSD != capMicro-testWorst+167_000 || l.Calls != 8 {
		t.Errorf("ledger %+v", l)
	}
}

func TestXSearchSpendCapJustOverRefuses(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	capMicro := int64(60_000_000)
	writeLedger(t, w, spendLedger{Version: 1, Month: "2026-11", MicroUSD: capMicro - testWorst + 1, Calls: 7})
	x.body = xaiBody("[]", []string{"https://x.com/alice/status/" + xid(1)}, "")
	res := w.run(opt)
	if x.calls() != 0 {
		t.Fatalf("one µ$ over the cap must not call xAI: %d calls", x.calls())
	}
	if !strings.Contains(w.out.String(), "xAI monthly spend cap: estimated $57.07 spent in 2026-11, next call's worst case $2.93 would pass the $60.00 cap; not calling") {
		t.Errorf("cap refusal not logged clearly:\n%s", w.out.String())
	}
	// The other kinds still post, and the run is not a failure.
	if res.ExitCode() != 0 || !reflect.DeepEqual(w.hub.firstLines("news"), []string{"https://rss.example/1"}) {
		t.Fatalf("exit %d, posted %v", res.ExitCode(), w.hub.firstLines("news"))
	}
	if l := readLedger(t, w); l.MicroUSD != capMicro-testWorst+1 || l.Calls != 7 {
		t.Errorf("a refused call changed the ledger: %+v", l)
	}
}

func TestXSearchCapIsConfigurable(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	w.cfg.XSearchMonthlyCapUSD = 2.9 // below one call's worst case
	x.body = xaiBody("[]", nil, "")
	w.run(opt)
	if x.calls() != 0 {
		t.Fatalf("a cap below one worst case must refuse every call: %d", x.calls())
	}
	c := &Config{Hub: "h.example", HubURL: "http://h.example", Channels: w.cfg.Channels}
	if err := c.Validate(); err != nil || c.XSearchMonthlyCapUSD != 60 {
		t.Fatalf("default cap %v (%v), want 60", c.XSearchMonthlyCapUSD, err)
	}
}

func TestXSearchNewMonthResets(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	writeLedger(t, w, spendLedger{Version: 1, Month: "2026-10", MicroUSD: 60_000_000, Calls: 400})
	x.body = xaiBody("[]", nil, "")
	w.run(opt)
	if l := readLedger(t, w); x.calls() != 1 || l.Month != "2026-11" || l.MicroUSD != 167_000 || l.Calls != 1 {
		t.Fatalf("calls %d ledger %+v", x.calls(), l)
	}
}

func TestXSearchWorstCaseChargedFirst(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	// No usage block: the call stays charged at its worst case.
	x.body = xaiBody("[]", nil, "null")
	w.run(opt)
	if l := readLedger(t, w); x.calls() != 1 || l.MicroUSD != testWorst {
		t.Fatalf("calls %d, ledger %+v: a call without usage must stay at its worst case", x.calls(), l)
	}
	// A 5xx may have done work: worst case. A 401 did none: released.
	w2, x2, opt2 := xworld(t, FeedConfig{Query: "mcp"})
	x2.status, x2.body = 503, `{"error":"overloaded"}`
	w2.run(opt2)
	if l := readLedger(t, w2); l.MicroUSD != testWorst {
		t.Errorf("after a 503 the ledger holds %d, want the worst case", l.MicroUSD)
	}
	w3, x3, opt3 := xworld(t, FeedConfig{Query: "mcp"})
	x3.status, x3.body = 401, `{"error":"bad key"}`
	w3.run(opt3)
	if l := readLedger(t, w3); l.MicroUSD != 0 || l.Calls != 1 {
		t.Errorf("after a 401 the ledger holds %+v, want 0 µ$", l)
	}
}

func TestXSearchInterval(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp", IntervalHours: 6})
	x.body = xaiBody("[]", nil, "")
	w.run(opt)
	w.now = w.now.Add(5 * time.Hour)
	w.run(opt)
	if x.calls() != 1 {
		t.Fatalf("called again inside interval_hours: %d calls", x.calls())
	}
	w.now = w.now.Add(time.Hour)
	w.run(opt)
	if x.calls() != 2 {
		t.Fatalf("not called after interval_hours: %d calls", x.calls())
	}
}

func TestXSearchRefusedWithoutStateOrInDryRun(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	x.body = xaiBody("[]", nil, "")
	w.run(opt, func(o *Options) { o.StatePath = "" })
	w.run(opt, func(o *Options) { o.DryRun = true })
	if x.calls() != 0 {
		t.Fatalf("xAI called without a ledger or in a dry run: %d", x.calls())
	}
}

func TestXSearchCorruptLedgerFailsClosed(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	if err := os.WriteFile(ledgerPath(w.statePath()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.body = xaiBody("[]", nil, "")
	res := w.run(opt)
	if x.calls() != 0 || res.ExitCode() != 1 {
		t.Fatalf("corrupt ledger: %d calls, exit %d", x.calls(), res.ExitCode())
	}
	if len(w.hub.posts("news")) != 1 {
		t.Errorf("rss should still post")
	}
}

func TestXSearchMissingKeyFileFailsFeedOnly(t *testing.T) {
	w, x, opt := xworld(t, FeedConfig{Query: "mcp"})
	res := w.run(opt, func(o *Options) { o.XAIKeyFile = filepath.Join(w.dir, "absent") })
	if x.calls() != 0 || len(w.hub.posts("news")) != 1 || res.FeedsFailed != 1 {
		t.Fatalf("calls %d posts %d failed %d", x.calls(), len(w.hub.posts("news")), res.FeedsFailed)
	}
	if l := readLedger(t, w); l.MicroUSD != 0 {
		t.Errorf("no call, but the ledger holds %d", l.MicroUSD)
	}
}

func TestParseXStatus(t *testing.T) {
	for u, want := range map[string][2]string{
		"https://x.com/alice/status/123":               {"123", "alice"},
		"https://twitter.com/Alice_1/status/123?s=20":  {"123", "Alice_1"},
		"https://mobile.twitter.com/a/statuses/9#frag": {"9", "a"},
		"https://x.com/i/status/42":                    {"42", ""},
		"https://x.com/i/web/status/42":                {"42", ""},
		"https://x.com/i/user/42":                      {"", ""},
		"https://evilx.com/a/status/1":                 {"", ""},
		"https://x.com.evil/a/status/1":                {"", ""},
	} {
		id, h := parseXStatus(u)
		if id != want[0] || h != want[1] {
			t.Errorf("parseXStatus(%q) = %q, %q; want %q, %q", u, id, h, want[0], want[1])
		}
	}
}

func TestXSearchConfigValidation(t *testing.T) {
	bad := []FeedConfig{
		{Kind: KindXSearch}, // neither handles nor query
		{Kind: KindXSearch, AllowedXHandles: []string{"@a"}}, // @
		{Kind: KindXSearch, Query: "q", URL: "https://x.example"},
		{Kind: KindXSearch, Query: "q", Model: "grok-unpriced"},
		{Kind: KindXSearch, Query: "q", MaxTurns: 6},
		{Kind: KindRSS, URL: "https://x.example", Query: "q"}, // xsearch field on rss
		{Kind: "atom", URL: "https://x.example"},
	}
	var hs []string
	for i := 0; i < 21; i++ {
		hs = append(hs, fmt.Sprintf("h%d", i))
	}
	bad = append(bad, FeedConfig{Kind: KindXSearch, AllowedXHandles: hs})
	for i, f := range bad {
		if err := f.validate(); err == nil {
			t.Errorf("bad[%d] %+v validated", i, f)
		}
	}
	ok := FeedConfig{Kind: KindXSearch, AllowedXHandles: hs[:20]}
	if err := ok.validate(); err != nil || ok.LookbackHours != 24 || ok.IntervalHours != 6 || ok.Model != "grok-4.7" || ok.MaxTurns != 2 {
		t.Errorf("20 handles: %v %+v", err, ok)
	}
}
