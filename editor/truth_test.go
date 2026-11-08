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
}

func newTruthFixture(t *testing.T, papers map[string]string, hnHits map[string][]map[string]any) *truthFixture {
	f := &truthFixture{runFixture: newRunFixture(t)}
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
		Now: func() time.Time { return t0.Add(50 * time.Hour) }, HFBase: f.hf.URL, HNBase: f.hn.URL})
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
// again but follows new thresholds; an unknown row is looked up again.
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
	rep := f.truth(t, false)
	if f.hfCalls != 5 { // B and C again, not A
		t.Fatalf("second pass hf calls = %d, want 5", f.hfCalls)
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

// evalCLIFixture writes a labels file whose model labels and popularity
// truth disagree everywhere, and returns options for Command("eval").
func evalCLIFixture(t *testing.T, agreeWithTruth bool) (CmdOpts, *fakeOR) {
	or := newFakeOR(t)
	or.probs["Hot"] = map[string]float64{"post": 0.9, "digest": 0.05, "drop": 0.05}
	or.probs["Cold"] = map[string]float64{"post": 0.1, "digest": 0.1, "drop": 0.8}
	cfg := &Config{HubURL: "https://hub.example", Sources: []string{"llm-papers"}, OpenRouterURL: or.srv.URL,
		Topics: []Topic{{Slug: "top-llm-papers", Title: "T", Describe: "d", From: []string{"llm-papers"}, About: "model operator"}}}
	cfg.Defaults()
	// The model labels say the decider is right (axis 2 perfect); the
	// truth says it is exactly wrong, unless agreeWithTruth.
	hot, cold := "drop", "post"
	if agreeWithTruth {
		hot, cold = "post", "drop"
	}
	labels := writeLabels(t,
		LabelRow{ID: "m_1", Kind: "item", Channel: "llm-papers", Title: "Hot", ModelLabel: "post"},
		LabelRow{ID: "m_2", Kind: "item", Channel: "llm-papers", Title: "Cold", ModelLabel: "drop"},
	)
	if agreeWithTruth { // and now axis 2 is exactly wrong
		labels = writeLabels(t,
			LabelRow{ID: "m_1", Kind: "item", Channel: "llm-papers", Title: "Hot", ModelLabel: "drop"},
			LabelRow{ID: "m_2", Kind: "item", Channel: "llm-papers", Title: "Cold", ModelLabel: "post"},
		)
	}
	v1, v2 := 50, 2
	truth := filepath.Join(t.TempDir(), "truth.json")
	raw, _ := json.Marshal([]TruthRow{
		{ID: "m_1", Channel: "llm-papers", Signal: SignalHF, Value: &v1, Label: hot},
		{ID: "m_2", Channel: "llm-papers", Signal: SignalHF, Value: &v2, Label: cold},
	})
	os.WriteFile(truth, raw, 0o600)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key\n"), 0o600)
	t.Setenv("OPENROUTER_API_KEY", "")
	return CmdOpts{Config: cfg, KeyFile: key, Labels: labels, Truth: truth, HTTP: or.srv.Client(),
		FloorAUC: 0.6, FloorPrecisionPost: 0.5, FloorRecallPopular: 0.5}, or
}

// TestEvalExitCodeIsAxis1Only: eval fails when the decisions miss the
// popularity truth even though they match the model labels perfectly, and
// passes when they meet the truth even though they miss every model label.
func TestEvalExitCodeIsAxis1Only(t *testing.T) {
	o, _ := evalCLIFixture(t, false)
	var out bytes.Buffer
	if code := Command(context.Background(), "eval", o, &out); code != 1 {
		t.Fatalf("decider wrong on truth, right on model labels: exit %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL axis 1: AUC 0.000 < floor 0.600") || !strings.Contains(out.String(), "axis2 label=post support=1 precision=1.000 recall=1.000") {
		t.Fatalf("output:\n%s", out.String())
	}
	o, _ = evalCLIFixture(t, true)
	out.Reset()
	if code := Command(context.Background(), "eval", o, &out); code != 0 {
		t.Fatalf("decider right on truth, wrong on model labels: exit %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "auc=1.000 precision_at_post=1.000 recall_popular=1.000") || strings.Contains(out.String(), "FAIL") {
		t.Fatalf("output:\n%s", out.String())
	}
	// Without -truth, a floor cannot be met.
	o.Truth = ""
	out.Reset()
	if code := Command(context.Background(), "eval", o, &out); code != 1 || !strings.Contains(out.String(), "no item has a popularity truth label") {
		t.Fatalf("no truth: exit %d\n%s", code, out.String())
	}
}

func TestChatDeciderUsesStructuredOutputAndGates(t *testing.T) {
	or := newFakeOR(t)
	or.chat = func(system, user string) string {
		if !strings.Contains(system, "About one item in eight") || !strings.Contains(user, "<data>") {
			return "{}"
		}
		if strings.Contains(user, "Hot") {
			return `{"post": 1.4, "digest": 0.4, "drop": 0.2}` // normalised: 0.7 0.2 0.1
		}
		return `{"post": 0.5, "digest": 0.3}`
	}
	cfg := DecideConfig{Post: 0.6, Digest: 0.5, Question: "v2", API: "chat"}
	topic := &Topic{Describe: "d"}
	m := or.model(1)
	d, err := DecideStory(context.Background(), m, "deepseek/deepseek-v4.1-flash", cfg, topic, &Story{ID: "s_1", Items: []StoryItem{{Title: "Hot"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Label != "post" || d.Probs["post"] < 0.6999 || d.Probs["post"] > 0.7001 || d.Probs["drop"] < 0.0999 || d.Probs["drop"] > 0.1001 {
		t.Fatalf("decision: %+v", d)
	}
	body := or.bodies[len(or.bodies)-1]
	rf, _ := body["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || body["reasoning"].(map[string]any)["enabled"] != false {
		t.Fatalf("request: %v", body)
	}
	if _, err := DecideStory(context.Background(), m, "deepseek/deepseek-v4.1-flash", cfg, topic, &Story{ID: "s_2", Items: []StoryItem{{Title: "Cold"}}}, 0); err == nil || !strings.Contains(err.Error(), `missing "drop"`) {
		t.Fatalf("missing probability: %v", err)
	}
}

func TestLabelDraftOnlyRedraftsTheSameRows(t *testing.T) {
	f := newRunFixture(t)
	items, _ := ReadSources(context.Background(), NewHub(f.hub.srv.URL, nil), f.cfg, &bytes.Buffer{})
	a, c := items[0].ID, items[2].ID
	only := map[string]bool{c: true, a + "+" + c: true, "m_gone+" + a: true}
	out := filepath.Join(t.TempDir(), "x.jsonl")
	f.or.chat = func(system, user string) string {
		data := user[strings.Index(user, "<data>")+7 : strings.Index(user, "</data>")]
		var rows []map[string]any
		json.Unmarshal([]byte(strings.TrimSpace(data)), &rows)
		l := "drop"
		if strings.Contains(system, "same underlying story") {
			l = "same"
		}
		var o []string
		for _, r := range rows {
			o = append(o, fmt.Sprintf(`{"id":%q,"label":%q,"reason":"r"}`, r["id"], l))
		}
		return strings.Join(o, "\n")
	}
	if err := LabelDraft(context.Background(), LabelOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), Model: f.or.model(1),
		Out: out, Source: "llm-papers", NItems: 10, NPairs: 10, Only: only, Log: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := ReadLabels(out)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != c+","+a+"+"+c {
		t.Fatalf("rows %v", ids)
	}
}
