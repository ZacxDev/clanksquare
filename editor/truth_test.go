package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTruthLabelPinned pins the upvote -> label mapping at the shipped
// thresholds, on each boundary and one past it on both sides.
func TestTruthLabelPinned(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults()
	tc := cfg.Truth
	if tc.HFPost != 40 || tc.HFDigest != 10 || tc.HNPost != 100 || tc.HNDigest != 10 || tc.MinAgeHours != 48 || tc.ReleaseLabel != "digest" {
		t.Fatalf("defaults changed: %+v", tc)
	}
	for _, c := range []struct {
		v    int
		want string
	}{
		{41, "post"}, {40, "post"}, {39, "digest"},
		{11, "digest"}, {10, "digest"}, {9, "drop"},
		{0, "drop"}, {144, "post"},
	} {
		if got := TruthLabel(c.v, tc.HFPost, tc.HFDigest); got != c.want {
			t.Errorf("hf %d upvotes = %q, want %q", c.v, got, c.want)
		}
	}
	for _, c := range []struct {
		v    int
		want string
	}{{101, "post"}, {100, "post"}, {99, "digest"}, {10, "digest"}, {9, "drop"}} {
		if got := TruthLabel(c.v, tc.HNPost, tc.HNDigest); got != c.want {
			t.Errorf("hn %d points = %q, want %q", c.v, got, c.want)
		}
	}
}

// truthFixture is the run fixture plus a news link, a GitHub release, and
// fake HF and HN servers.
type truthFixture struct {
	*runFixture
	hf, hn  *httptest.Server
	hfCalls int
	at      time.Duration // now, after t0 (default 50h)
}

func newTruthFixture(t *testing.T, papers map[string]string, hnHits map[string][]map[string]any) *truthFixture {
	f := &truthFixture{runFixture: newRunFixture(t), at: 50 * time.Hour}
	rawPost(t, f.hub, f.curator, f.rawID, "News", "https://news.example/story", "A story.", ms(-3*time.Hour))
	rawPost(t, f.hub, f.curator, f.rawID, "Big news", "https://news.example/big", "A big story.", ms(-3*time.Hour))
	rawPost(t, f.hub, f.curator, f.rawID, "Release", "https://github.com/o/r/releases/tag/v1", "v1.", ms(-3*time.Hour))
	rawPost(t, f.hub, f.curator, f.rawID, "Fresh", "https://arxiv.org/abs/2611.00009v1", "New.", ms(10*time.Hour))
	f.hf = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hfCalls++
		id := strings.TrimPrefix(r.URL.Path, "/api/papers/")
		body, ok := papers[id]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"Paper not found."}`))
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(f.hf.Close)
	f.hn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits := hnHits[r.URL.Query().Get("query")]
		if hits == nil {
			hits = []map[string]any{}
		}
		okJSON(w, map[string]any{"hits": hits})
	}))
	t.Cleanup(f.hn.Close)
	return f
}

func (f *truthFixture) truth(t *testing.T, refresh bool) *TruthReport {
	t.Helper()
	rep, err := Truth(context.Background(), TruthOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), StateDir: f.dir, Refresh: refresh,
		Now: func() time.Time { return t0.Add(f.at) }, HFBase: f.hf.URL, HNBase: f.hn.URL})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func byTitle(t *testing.T, f *truthFixture, rows map[string]TruthRow) map[string]TruthRow {
	t.Helper()
	items, err := ReadSources(context.Background(), NewHub(f.hub.srv.URL, nil), f.cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]TruthRow{}
	for _, it := range items {
		out[it.Title] = rows[it.ID]
	}
	return out
}

// TestTruthNoSignalIsUnknown: an item with no signal (not on HF, on HF but
// never on Daily Papers, never submitted to HN, too new) is unknown, not
// drop. A measured low value is drop.
func TestTruthNoSignalIsUnknown(t *testing.T) {
	daily := "2026-11-08T00:00:00.000Z" // t0 is 2026-11-08 12:00; now is +50h
	f := newTruthFixture(t, map[string]string{
		"2610.00001": fmt.Sprintf(`{"id":"2610.00001","upvotes":40,"submittedOnDailyAt":%q}`, daily),
		"2610.00002": `{"id":"2610.00002","upvotes":0}`, // a page, never on Daily Papers
		// 2610.00003: 404
		"2611.00009": fmt.Sprintf(`{"id":"2611.00009","upvotes":99,"submittedOnDailyAt":%q}`, daily), // too new: never asked
	}, map[string][]map[string]any{
		"news.example/big": {{"url": "https://news.example/big", "points": 3}},
	})
	rep := f.truth(t, false)
	got := byTitle(t, f, rep.Rows)
	want := map[string][2]string{ // title -> {label, why}
		"Paper A":  {"post", ""},
		"Paper B":  {"", "not_on_daily"},
		"Paper C":  {"", "not_on_hf"},
		"News":     {"", "not_on_hn"},
		"Big news": {"drop", ""},
		"Release":  {"digest", ""},
		"Fresh":    {"", "too_new"},
	}
	for title, w := range want {
		r := got[title]
		if r.Label != w[0] || r.Why != w[1] {
			t.Errorf("%s: label %q why %q, want %q %q", title, r.Label, r.Why, w[0], w[1])
		}
		if (r.Label == "" || r.Signal == SignalRelease) != (r.Value == nil) {
			t.Errorf("%s: value %v with label %q signal %q", title, r.Value, r.Label, r.Signal)
		}
	}
	if f.hfCalls != 3 {
		t.Errorf("hf calls = %d, want 3 (the too-new paper is not asked)", f.hfCalls)
	}
	if rep.Labelled["llm-papers"] != 3 || rep.Eligible["llm-papers"] != 6 {
		t.Errorf("coverage: labelled %d eligible %d", rep.Labelled["llm-papers"], rep.Eligible["llm-papers"])
	}
	// The cache holds no item text.
	raw, _ := os.ReadFile(TruthPath(f.dir))
	for _, s := range []string{"Paper A", "About agents", "A big story"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("truth file holds item text %q", s)
		}
	}
}

// TestTruthCacheRelabelsAndRetriesUnknown: a cached label is not looked up
// again but follows new thresholds; an unknown row is looked up again once
// its answer is recheck_hours (20) old, and not before.
func TestTruthCacheRelabelsAndRetriesUnknown(t *testing.T) {
	daily := "2026-11-08T00:00:00.000Z"
	papers := map[string]string{"2610.00001": fmt.Sprintf(`{"id":"2610.00001","upvotes":40,"submittedOnDailyAt":%q}`, daily)}
	f := newTruthFixture(t, papers, nil)
	f.truth(t, false)
	if f.hfCalls != 3 {
		t.Fatalf("first pass hf calls = %d", f.hfCalls)
	}
	f.cfg.Truth.HFPost = 41
	papers["2610.00003"] = fmt.Sprintf(`{"id":"2610.00003","upvotes":12,"submittedOnDailyAt":%q}`, daily)
	// 1 ms short of recheck_hours: B and C (unknown) are not asked again.
	f.at += 20*time.Hour - time.Millisecond
	// The one hf call is Fresh, now old enough (too_new is no answer).
	if rep := f.truth(t, false); f.hfCalls != 4 || rep.Deferred != 4 { // B, C, News, Big news
		t.Fatalf("pass inside recheck_hours: hf calls = %d (want 4), deferred = %d (want 4)", f.hfCalls, rep.Deferred)
	}
	// At recheck_hours they are asked again (Fresh's answer is younger).
	f.at += time.Millisecond
	rep := f.truth(t, false)
	if f.hfCalls != 6 { // B and C again, not A, not Fresh
		t.Fatalf("pass at recheck_hours: hf calls = %d, want 6", f.hfCalls)
	}
	got := byTitle(t, f, rep.Rows)
	if got["Paper A"].Label != "digest" || got["Paper C"].Label != "digest" {
		t.Fatalf("A %+v C %+v", got["Paper A"], got["Paper C"])
	}
	saved, err := ReadTruth(filepath.Join(f.dir, "truth.json"))
	if err != nil || saved[got["Paper A"].ID].Label != "digest" {
		t.Fatalf("saved: %v %+v", err, saved[got["Paper A"].ID])
	}
}

// evalCLIFixture: two stories with popularity truth, Hot (truth post) and
// Cold (truth drop), decided from the hub window. With agreeWithTruth the
// decider gives Hot the higher P(post), else Cold. k = 1 per UTC day.
func evalCLIFixture(t *testing.T, agreeWithTruth bool) (CmdOpts, *fakeOR) {
	f := newRunFixture(t)
	hot := rawPost(t, f.hub, f.curator, f.rawID, "Hot", "https://arxiv.org/abs/2610.00011v1", "Hot.", ms(-5*time.Hour))
	cold := rawPost(t, f.hub, f.curator, f.rawID, "Cold", "https://arxiv.org/abs/2610.00012v1", "Cold.", ms(-4*time.Hour))
	hp, cp := 0.1, 0.9
	if agreeWithTruth {
		hp, cp = 0.9, 0.1
	}
	f.or.probs["Hot"] = map[string]float64{"post": hp, "digest": 0, "drop": 1 - hp}
	f.or.probs["Cold"] = map[string]float64{"post": cp, "digest": 0, "drop": 1 - cp}
	// The run fixture's A, B, C (no truth) rank below both, so they never
	// take the day's one pick.
	for i, title := range []string{"Paper A", "Paper B", "Paper C"} {
		p := 0.05 - 0.01*float64(i)
		f.or.probs[title] = map[string]float64{"post": p, "digest": 0, "drop": 1 - p}
	}
	f.cfg.Decide.TopKPerDay = 1
	f.cfg.OpenRouterURL = f.or.srv.URL
	v1, v2 := 50, 2
	truth := filepath.Join(t.TempDir(), "truth.json")
	raw, _ := json.Marshal([]TruthRow{
		{ID: hot, Channel: "llm-papers", URL: "https://arxiv.org/abs/2610.00011", Signal: SignalHF, Value: &v1, Label: "post"},
		{ID: cold, Channel: "llm-papers", URL: "https://arxiv.org/abs/2610.00012", Signal: SignalHF, Value: &v2, Label: "drop"},
	})
	os.WriteFile(truth, raw, 0o600)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key\n"), 0o600)
	t.Setenv("OPENROUTER_API_KEY", "")
	return CmdOpts{Config: f.cfg, KeyFile: key, Truth: truth, HTTP: f.hub.srv.Client(), Now: func() time.Time { return t0 },
		FloorAUC: 0.6, FloorPrecisionAtK: 0.5, FloorRecallPopular: 0.5}, f.or
}

// TestEvalExitCodeIsAxis1Only: eval's exit status follows the floors on the
// rank pick against the popularity truth (the only axis left): it fails when
// the decider ranks the truth-popular story below the unpopular one, passes
// when it ranks it above, and a floor cannot be met without -truth.
func TestEvalExitCodeIsAxis1Only(t *testing.T) {
	o, _ := evalCLIFixture(t, false)
	var out bytes.Buffer
	if code := Command(context.Background(), "eval", o, &out); code != 1 {
		t.Fatalf("decider wrong on truth: exit %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL eval: AUC 0.000 < floor 0.600") || !strings.Contains(out.String(), "FAIL eval: precision@k 0.000 < floor 0.500") {
		t.Fatalf("output:\n%s", out.String())
	}
	o, _ = evalCLIFixture(t, true)
	out.Reset()
	if code := Command(context.Background(), "eval", o, &out); code != 0 {
		t.Fatalf("decider right on truth: exit %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "precision_at_k=1.000 recall_popular=1.000 auc=1.000") || strings.Contains(out.String(), "FAIL") {
		t.Fatalf("output:\n%s", out.String())
	}
	// Without -truth, a floor cannot be met.
	o.Truth = ""
	out.Reset()
	if code := Command(context.Background(), "eval", o, &out); code != 1 || !strings.Contains(out.String(), "no story has a popularity truth label") {
		t.Fatalf("no truth: exit %d\n%s", code, out.String())
	}
}
