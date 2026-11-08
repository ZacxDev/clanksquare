package editor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

func storiesOf(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Story)
	}
	return out
}

// rankFixture: six candidates with pairwise distinct P(post), none equal to
// the floor or budget used below, given in an order unrelated to their rank.
var rankFixture = []Candidate{
	{"s_c", 0.41}, {"s_f", 0.07}, {"s_a", 0.93}, {"s_e", 0.18}, {"s_b", 0.66}, {"s_d", 0.29},
}

// TestRankPickBudgetBoundary pins the literal pick at k = 4 (the default
// budget) and k + 1 = 5.
func TestRankPickBudgetBoundary(t *testing.T) {
	if got := storiesOf(RankPick(rankFixture, 4, 0)); !reflect.DeepEqual(got, []string{"s_a", "s_b", "s_c", "s_d"}) {
		t.Errorf("k=4: %v", got)
	}
	if got := storiesOf(RankPick(rankFixture, 5, 0)); !reflect.DeepEqual(got, []string{"s_a", "s_b", "s_c", "s_d", "s_e"}) {
		t.Errorf("k=5: %v", got)
	}
	if got := RankPick(rankFixture, 0, 0); len(got) != 0 {
		t.Errorf("k=0 picked %v", storiesOf(got))
	}
	if got := RankPick(rankFixture, -2, 0); len(got) != 0 {
		t.Errorf("k=-2 (over budget) picked %v", storiesOf(got))
	}
	cfg := &Config{}
	cfg.Defaults()
	if cfg.Decide.TopKPerDay != 4 || cfg.Decide.MinPPost != 0 || cfg.Decide.WindowHours != 72 ||
		cfg.DecisionModel != "perplexity/pplx-decider-v1.2" || cfg.Decide.Question != "v2" {
		t.Errorf("defaults changed: model %s %+v", cfg.DecisionModel, cfg.Decide)
	}
}

// TestRankPickTieBreaksByStoryID: equal P(post) straddling the budget
// boundary is settled by story id, whatever the input order.
func TestRankPickTieBreaksByStoryID(t *testing.T) {
	a := []Candidate{{"s_top", 0.9}, {"s_zz", 0.5}, {"s_mm", 0.5}, {"s_aa", 0.5}, {"s_low", 0.1}}
	b := []Candidate{{"s_aa", 0.5}, {"s_low", 0.1}, {"s_mm", 0.5}, {"s_top", 0.9}, {"s_zz", 0.5}}
	for _, in := range [][]Candidate{a, b} {
		if got := storiesOf(RankPick(in, 3, 0)); !reflect.DeepEqual(got, []string{"s_top", "s_aa", "s_mm"}) {
			t.Errorf("input %v: picked %v, want [s_top s_aa s_mm]", storiesOf(in), got)
		}
	}
}

// TestRankPickFloor: the optional floor posts fewer on a weak day; at the
// floor is in, just under is out.
func TestRankPickFloor(t *testing.T) {
	if got := storiesOf(RankPick(rankFixture, 4, 0.5)); !reflect.DeepEqual(got, []string{"s_a", "s_b"}) {
		t.Errorf("floor 0.5: %v", got)
	}
	if got := storiesOf(RankPick(rankFixture, 4, 0.66)); !reflect.DeepEqual(got, []string{"s_a", "s_b"}) {
		t.Errorf("floor at s_b's 0.66: %v", got)
	}
	if got := storiesOf(RankPick(rankFixture, 4, 0.661)); !reflect.DeepEqual(got, []string{"s_a"}) {
		t.Errorf("floor just over s_b: %v", got)
	}
	if got := RankPick(rankFixture, 4, 0.95); len(got) != 0 {
		t.Errorf("floor over all: %v", storiesOf(got))
	}
}

func TestPostedTodayCountsUTCDayAndPending(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 30, 0, 0, time.UTC)
	start := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC).UnixMilli()
	cs := &ChannelState{Seen: map[string]int64{
		"s_today0": start, "s_today1": start + 1, "s_yday": start - 1, "s_tomorrow": start + 24*3600_000,
	}, Pending: map[string]string{"s_p": "tok", "s_today1": "tok"}}
	// s_today0, s_today1 (also pending: counted once), s_p.
	if n := PostedToday(cs, now); n != 3 {
		t.Fatalf("posted today = %d, want 3", n)
	}
}

// runAt runs the fixture at a given time.
func (f *runFixture) runAt(at time.Time, post, preview bool) (*Result, string) {
	var out bytes.Buffer
	res := Run(context.Background(), RunOptions{Config: f.cfg, Key: f.editor, StateDir: f.dir, Post: post, Preview: preview,
		Hub: NewHub(f.hub.srv.URL, f.hub.srv.Client()), Model: f.or.model(1), Now: func() time.Time { return at }, Out: &out})
	return res, out.String()
}

func postedStories(t *testing.T, f *runFixture) []string {
	t.Helper()
	var out []string
	for _, p := range f.hub.posts("top-llm-papers") {
		h, err := ParseStory(p.Fields["body"].(string))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.URL)
	}
	return out
}

// TestRunBudgetSurvivesStateLoss: with the state file gone, today's posts are
// counted from the channel's log, so a restart on the same UTC day posts
// nothing new however good the new stories are; the next day posts again.
func TestRunBudgetSurvivesStateLoss(t *testing.T) {
	f := newRunFixture(t) // budget 2
	res, out := f.run(true, nil)
	if res.ExitCode() != 0 || len(f.hub.posts("top-llm-papers")) != 2 {
		t.Fatalf("day 1: exit %d posts %d\n%s", res.ExitCode(), len(f.hub.posts("top-llm-papers")), out)
	}
	// Two better stories arrive the same day, and the state file is lost.
	rawPost(t, f.hub, f.curator, f.rawID, "Paper D", "https://arxiv.org/abs/2610.00004v1", "About agents D.", ms(-30*time.Minute))
	rawPost(t, f.hub, f.curator, f.rawID, "Paper E", "https://arxiv.org/abs/2610.00005v1", "About agents E.", ms(-20*time.Minute))
	f.or.vecs["Paper D"], f.or.vecs["Paper E"] = unitVec(0, 0, 0, 1), unitVec(0, 0, 0, 0, 1)
	f.or.probs["Paper D"] = map[string]float64{"post": 0.97, "digest": 0.02, "drop": 0.01}
	f.or.probs["Paper E"] = map[string]float64{"post": 0.95, "digest": 0.03, "drop": 0.02}
	if err := os.Remove(StatePath(f.dir)); err != nil {
		t.Fatal(err)
	}
	res, out = f.run(true, nil)
	if n := len(f.hub.posts("top-llm-papers")); res.ExitCode() != 0 || n != 2 {
		t.Fatalf("same day after state loss: exit %d, %d posts, want 2 (budget counted from the log)\n%s", res.ExitCode(), n, out)
	}
	if !strings.Contains(out, "eligible=3 posted_today=2 budget=2") {
		t.Fatalf("output:\n%s", out)
	}
	// The next UTC day: the budget is back, and D and E (the best eligible)
	// go out; B is still eligible (26h old, inside 72h) but ranks below.
	res, out = f.runAt(t0.Add(14*time.Hour), true, false)
	got := postedStories(t, f)
	want := []string{"https://arxiv.org/abs/2610.00001", "https://arxiv.org/abs/2610.00003", "https://arxiv.org/abs/2610.00004", "https://arxiv.org/abs/2610.00005"}
	if res.ExitCode() != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("day 2: exit %d posts %v\n%s", res.ExitCode(), got, out)
	}
}

// TestRunUnpickedStaysEligibleInsideWindow: B, not picked on day 1, is
// picked on a later day while its first item is younger than window_hours,
// and never once it is older.
func TestRunUnpickedStaysEligibleInsideWindow(t *testing.T) {
	f := newRunFixture(t)   // B created at t0-2h
	f.cfg.MaxAgeHours = 168 // read old items, so only the pick window decides
	if res, out := f.run(true, nil); res.ExitCode() != 0 || len(f.hub.posts("top-llm-papers")) != 2 {
		t.Fatalf("day 1: %s", out)
	}
	// t0-2h+72h = t0+70h: one ms before, B is eligible.
	res, out := f.runAt(t0.Add(70*time.Hour-time.Millisecond), false, false)
	if res.Topics[0].Eligible != 1 || !strings.Contains(out, `would post "Paper B"`) {
		t.Fatalf("inside window: %+v\n%s", res.Topics[0], out)
	}
	res, out = f.runAt(t0.Add(70*time.Hour+time.Millisecond), false, false)
	if res.Topics[0].Eligible != 0 || strings.Contains(out, "would post") {
		t.Fatalf("outside window: %+v\n%s", res.Topics[0], out)
	}
}

// TestRunFloorPostsFewer: with min_p_post above C's 0.8, only A posts.
func TestRunFloorPostsFewer(t *testing.T) {
	f := newRunFixture(t)
	f.cfg.Decide.MinPPost = 0.85
	res, out := f.run(true, nil)
	if got := postedStories(t, f); res.ExitCode() != 0 || !reflect.DeepEqual(got, []string{"https://arxiv.org/abs/2610.00001"}) {
		t.Fatalf("posts %v\n%s", got, out)
	}
}

func readPreview(t *testing.T, path string) []PreviewRow {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var rows []PreviewRow
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var r PreviewRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	return rows
}

// TestRunPreviewKeepsDecisionsAndPicks: a preview submits nothing, never
// writes the live state file, keeps its own state (so no story is decided
// twice and yesterday's picks are not picked again), writes a dated JSONL of
// every decision and pick, and prints one summary line.
func TestRunPreviewKeepsDecisionsAndPicks(t *testing.T) {
	f := newRunFixture(t) // budget 2
	res, out := f.runAt(t0, false, true)
	if res.ExitCode() != 0 {
		t.Fatalf("exit %d\n%s", res.ExitCode(), out)
	}
	if n := f.hub.count("msg") + f.hub.count("channel"); n != 0 {
		t.Fatalf("preview submitted %d object(s)", n)
	}
	if _, err := os.Stat(StatePath(f.dir)); !os.IsNotExist(err) {
		t.Fatalf("preview wrote the live state file: %v", err)
	}
	if _, err := os.Stat(StatePath(PreviewDir(f.dir))); err != nil {
		t.Fatalf("preview state: %v", err)
	}
	path := PreviewFile(f.dir, "2026-11-08")
	rows := readPreview(t, path)
	type rs struct {
		Rank   int
		Status string
		URL    string
		PPost  float64
	}
	var got []rs
	for _, r := range rows {
		got = append(got, rs{r.Rank, r.Status, r.URL, r.Probs["post"]})
	}
	want := []rs{
		{1, "picked", "https://arxiv.org/abs/2610.00001", 0.9},
		{2, "picked", "https://arxiv.org/abs/2610.00003", 0.8},
		{3, "not_picked", "https://arxiv.org/abs/2610.00002", 0.45},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preview rows %+v, want %+v", got, want)
	}
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "PREVIEW-SUMMARY ") {
			lines++
			if !strings.Contains(l, "date=2026-11-08") || !strings.Contains(l, "top-llm-papers:decided=3,eligible=3,today=0,picked=2,budget=2") || !strings.Contains(l, "file="+path) {
				t.Fatalf("summary: %s", l)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("%d summary lines\n%s", lines, out)
	}
	// Same day again: nothing new is decided or picked.
	dec := f.or.count("decisions")
	res, out = f.runAt(t0.Add(time.Hour), false, true)
	if f.or.count("decisions") != dec || res.Topics[0].Posted != 0 || res.Topics[0].Today != 2 {
		t.Fatalf("second preview same day: decisions %d->%d %+v\n%s", dec, f.or.count("decisions"), res.Topics[0], out)
	}
	// Next day: the budget is back; A and C are not picked again, B is.
	res, out = f.runAt(t0.Add(13*time.Hour), false, true)
	if res.Topics[0].Posted != 1 || !strings.Contains(out, `PREVIEW channel=top-llm-papers would post "Paper B"`) {
		t.Fatalf("day 2: %+v\n%s", res.Topics[0], out)
	}
	if rows := readPreview(t, PreviewFile(f.dir, "2026-11-09")); len(rows) != 3 || rows[2].Status != "picked" || rows[0].Status != "posted" {
		t.Fatalf("day 2 rows: %+v", rows)
	}
	if n := f.hub.count("msg") + f.hub.count("channel"); n != 0 {
		t.Fatalf("preview submitted %d object(s)", n)
	}
}

func TestRunPreviewRefusesPostAndNeedsState(t *testing.T) {
	f := newRunFixture(t)
	res, out := f.runAt(t0, true, true)
	if res.ExitCode() != 1 || !strings.Contains(out, "-preview and -post are exclusive") || f.hub.count("msg") != 0 {
		t.Fatalf("exit %d\n%s", res.ExitCode(), out)
	}
	f.dir = ""
	res, out = f.runAt(t0, false, true)
	if res.ExitCode() != 1 || !strings.Contains(out, "-preview needs -state") {
		t.Fatalf("exit %d\n%s", res.ExitCode(), out)
	}
}

// ---- HF Daily Papers: circular truth ----

// TestTruthExcludesHFDailyOnlyItems: an item whose only source is HF Daily
// Papers is excluded from axis 1 (its upvotes would grade the source that
// picked it); the same paper also carried by another source is not.
func TestTruthExcludesHFDailyOnlyItems(t *testing.T) {
	daily := "2026-11-08T00:00:00.000Z"
	papers := map[string]string{
		"2610.00001": `{"id":"2610.00001","upvotes":40,"submittedOnDailyAt":"` + daily + `"}`,
		"2610.00007": `{"id":"2610.00007","upvotes":60,"submittedOnDailyAt":"` + daily + `"}`,
		"2610.00008": `{"id":"2610.00008","upvotes":70,"submittedOnDailyAt":"` + daily + `"}`,
	}
	f := newTruthFixture(t, papers, nil)
	post := func(title, link, via string) string {
		body := link + "\n\nAbout it.\n\nvia " + via
		tok := clankmail.Sign(f.curator, hubkit.MsgPayload(f.curator, f.rawID, title, body, ms(-3*time.Hour)))
		f.hub.mu.Lock()
		f.hub.logs[f.rawID] = append(f.hub.logs[f.rawID], tok)
		f.hub.mu.Unlock()
		return hubkit.TokenID(tok)
	}
	onlyHF := post("HF only", "https://arxiv.org/abs/2610.00007", HFDailyVia)
	bothHF := post("HF too", "https://arxiv.org/abs/2610.00008", HFDailyVia)
	bothArxiv := post("arXiv too", "http://arxiv.org/abs/2610.00008v2", "arXiv (cs.CL)")
	rep := f.truth(t, false)
	for id, want := range map[string]string{onlyHF: ExcludedHFDailyOnly, bothHF: "", bothArxiv: ""} {
		r := rep.Rows[id]
		if r.Excluded != want || r.Label != "post" {
			t.Errorf("%s: excluded %q label %q, want excluded %q label post", id, r.Excluded, r.Label, want)
		}
	}
	if rep.Excluded != 1 {
		t.Errorf("report excluded = %d, want 1", rep.Excluded)
	}
	saved, err := ReadTruth(TruthPath(f.dir))
	if err != nil || saved[onlyHF].Excluded != ExcludedHFDailyOnly {
		t.Fatalf("saved: %v %+v", err, saved[onlyHF])
	}
}

// TestEvalAxis1SkipsExcludedRows: an excluded truth row is not scored on
// axis 1, even when its label is post, and is counted as excluded.
func TestEvalAxis1SkipsExcludedRows(t *testing.T) {
	or := newFakeOR(t)
	or.probs["Hot"] = map[string]float64{"post": 0.9, "digest": 0.05, "drop": 0.05}
	or.probs["Cold"] = map[string]float64{"post": 0.1, "digest": 0.1, "drop": 0.8}
	or.probs["Circular"] = map[string]float64{"post": 0.05, "digest": 0.1, "drop": 0.85}
	cfg := &Config{HubURL: "https://hub.example", Sources: []string{"llm-papers"},
		Topics: []Topic{{Slug: "top-llm-papers", Title: "T", Describe: "d", From: []string{"llm-papers"}, About: "model operator"}}}
	cfg.Defaults()
	labels := writeLabels(t,
		LabelRow{ID: "m_1", Kind: "item", Channel: "llm-papers", Title: "Hot", ModelLabel: "post"},
		LabelRow{ID: "m_2", Kind: "item", Channel: "llm-papers", Title: "Cold", ModelLabel: "drop"},
		LabelRow{ID: "m_3", Kind: "item", Channel: "llm-papers", Title: "Circular", ModelLabel: "drop"},
	)
	v := 50
	truth := map[string]TruthRow{
		"m_1": {ID: "m_1", Signal: SignalHF, Value: &v, Label: "post"},
		"m_2": {ID: "m_2", Signal: SignalHF, Value: &v, Label: "drop"},
		"m_3": {ID: "m_3", Signal: SignalHF, Value: &v, Label: "post", Excluded: ExcludedHFDailyOnly},
	}
	rep, err := Eval(context.Background(), EvalOptions{Config: cfg, Model: or.model(1), Labels: labels, Truth: truth})
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Axis1
	// Scored: m_1 (post, P 0.9) and m_2 (drop, P 0.1): AUC 1. With m_3 (post,
	// P 0.05, below m_2) scored too, AUC would be 0.5 and popular 2.
	if a.Rows != 2 || a.Excluded != 1 || a.Unknown != 0 || a.Positive != 1 || a.AUC != 1 {
		t.Fatalf("axis1 rows %d excluded %d unknown %d popular %d auc %.3f", a.Rows, a.Excluded, a.Unknown, a.Positive, a.AUC)
	}
}
