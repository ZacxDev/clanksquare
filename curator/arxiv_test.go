package curator

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Every version and page form of an arXiv paper is one key; anything that is
// not a paper page keeps the ordinary normalisation.
func TestNormalizeLinkCollapsesArxivVersions(t *testing.T) {
	for in, want := range map[string]string{
		// new-style ids, every page form and version
		"https://arxiv.org/abs/2610.40641v1":            "https://arxiv.org/abs/2610.40641",
		"http://arxiv.org/abs/2610.40641v12":            "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/abs/2610.40641":              "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/pdf/2610.40641v2":            "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/pdf/2610.40641v3.pdf":        "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/pdf/2610.40641":              "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/html/2610.40641v2":           "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/html/2610.40641v2/":          "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/html/2610.40641v2#S3":        "https://arxiv.org/abs/2610.40641",
		"https://WWW.arXiv.org/abs/2610.40641v4":        "https://arxiv.org/abs/2610.40641",
		"https://export.arxiv.org/abs/2610.40641v1":     "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/abs/2610.40641v2?context=cs": "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/abs/2610.40641?utm_source=x": "https://arxiv.org/abs/2610.40641",
		"https://arxiv.org/abs/0704.0001v1":             "https://arxiv.org/abs/0704.0001",
		// old-style ids
		"https://arxiv.org/abs/hep-th/9901001v2":    "https://arxiv.org/abs/hep-th/9901001",
		"https://arxiv.org/abs/hep-th/9901001":      "https://arxiv.org/abs/hep-th/9901001",
		"http://arxiv.org/pdf/hep-th/9901001v1.pdf": "https://arxiv.org/abs/hep-th/9901001",
		"https://arxiv.org/abs/math.GT/0309136v3":   "https://arxiv.org/abs/math.GT/0309136",
		"https://arxiv.org/abs/cond-mat/0102536v1":  "https://arxiv.org/abs/cond-mat/0102536",
		"https://arxiv.org/abs/cs/0112017":          "https://arxiv.org/abs/cs/0112017",
		// not paper pages: ordinary normalisation only
		"https://arxiv.org/list/cs.AI/recent":        "https://arxiv.org/list/cs.AI/recent",
		"https://arxiv.org/html/2610.40641v2/x1.png": "https://arxiv.org/html/2610.40641v2/x1.png",
		"https://arxiv.org/abs/2610.40641v2extra":    "https://arxiv.org/abs/2610.40641v2extra",
		"https://arxiv.org/abs/261.40641v2":          "https://arxiv.org/abs/261.40641v2",
		"https://arxiv.org/format/2610.40641v2":      "https://arxiv.org/format/2610.40641v2",
		"https://arxiv.org:8443/abs/2610.40641v2":    "https://arxiv.org:8443/abs/2610.40641v2",
		"https://notarxiv.org/abs/2610.40641v2":      "https://notarxiv.org/abs/2610.40641v2",
		"https://arxiv.org.example/abs/2610.40641v2": "https://arxiv.org.example/abs/2610.40641v2",
		"https://ARXIV.ORG/abs/hep-th/990100v2":      "https://arxiv.org/abs/hep-th/990100v2",
	} {
		if got := NormalizeLink(in); got != want {
			t.Errorf("NormalizeLink(%q) = %q, want %q", in, got, want)
		}
		if got := NormalizeLink(want); got != want {
			t.Errorf("not idempotent on %q: %q", want, got)
		}
	}
}

// A state file written before the arXiv collapse is migrated on load: every
// version of a paper becomes one key with the earliest first-seen time, other
// keys are untouched, and pending entries collapse the same way.
func TestLoadStateRekeysArxivVersions(t *testing.T) {
	p := t.TempDir() + "/state.json"
	old := `{"version":1,"channels":{"c_x":{"slug":"papers",` +
		`"seen":{"https://arxiv.org/abs/2610.40641v1":5000,"http://arxiv.org/pdf/2610.40641v2":3000,` +
		`"https://arxiv.org/abs/hep-th/9901001v2":9000,"https://x.example/a":7000,"guid:abc":1000},` +
		`"pending":{"https://arxiv.org/abs/2611.00002v2":"tokB","https://arxiv.org/abs/2611.00002v1":"tokA"}}}}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(p)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	cs := s.Channels["c_x"]
	wantSeen := map[string]int64{
		"https://arxiv.org/abs/2610.40641":     3000,
		"https://arxiv.org/abs/hep-th/9901001": 9000,
		"https://x.example/a":                  7000,
		"guid:abc":                             1000,
	}
	if !reflect.DeepEqual(cs.Seen, wantSeen) {
		t.Errorf("seen %v\nwant %v", cs.Seen, wantSeen)
	}
	if want := map[string]string{"https://arxiv.org/abs/2611.00002": "tokA"}; !reflect.DeepEqual(cs.Pending, want) {
		t.Errorf("pending %v, want %v", cs.Pending, want)
	}
}

// End to end through the state file: a paper posted at v1 by an older binary
// is not posted again when the feed later lists it at v2.
func TestArxivV2NotRepostedAfterV1FromState(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/other", title: "other", date: ago(3)}))
	w.config(5, feed("/a"))
	w.run() // creates the channel
	id := w.hub.slugs["news"]
	old := `{"version":1,"channels":{"` + id + `":{"slug":"news","seen":{"https://x.example/other":1,"https://arxiv.org/abs/2610.40641v1":2}}}}`
	if err := os.WriteFile(w.statePath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/other", title: "other", date: ago(3)},
		entry{id: "2", link: "https://arxiv.org/abs/2610.40641v2", title: "paper v2", date: ago(1)},
		entry{id: "3", link: "https://arxiv.org/abs/2610.50000v2", title: "new paper", date: ago(1)}))
	w.now = w.now.Add(time.Hour)
	w.run()
	got := w.hub.firstLines("news")
	if want := []string{"https://x.example/other", "https://arxiv.org/abs/2610.50000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("posted %v, want %v: v2 of a paper seen at v1 must not post again", got, want)
	}
}

// The same through the log rebuild (no state file): an old post whose first
// line is the v1 link keys the paper, so its v2 does not post again.
func TestArxivV2NotRepostedAfterV1FromLog(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/other", title: "other", date: ago(3)}))
	w.config(5, feed("/a"))
	w.run()
	cid := w.hub.slugs["news"]
	me := testKey("curator")
	w.hub.appendLog("news", clankmail.Sign(me, msgPayload(me, cid, "paper v1", "https://arxiv.org/abs/2610.40641v1\n\nvia arXiv", testNow.UnixMilli())))
	if err := os.Remove(w.statePath()); err != nil {
		t.Fatal(err)
	}
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "2", link: "https://arxiv.org/pdf/2610.40641v2", title: "paper v2", date: ago(1)},
		entry{id: "3", link: "https://arxiv.org/abs/2610.50000v2", title: "new paper", date: ago(1)}))
	w.now = w.now.Add(time.Hour)
	w.run()
	var ours []string
	for _, p := range w.hub.posts("news") {
		line, _, _ := strings.Cut(p.Fields["body"].(string), "\n")
		ours = append(ours, line)
	}
	want := []string{"https://x.example/other", "https://arxiv.org/abs/2610.40641v1", "https://arxiv.org/abs/2610.50000"}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("log %v, want %v: the rebuilt v1 key must cover v2", ours, want)
	}
}
