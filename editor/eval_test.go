package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestBelowFloorsAxis1Only: the floors apply to the rank pick's scores
// against the popularity truth (the only axis eval has), and a report that
// cannot be scored fails a floor instead of passing it.
func TestBelowFloorsAxis1Only(t *testing.T) {
	r := &EvalReport{Scored: 4, Positive: 2, AUC: 0.75, PrecisionAtK: 0.5, RecallPopular: 0.5}
	if f := r.BelowFloors(Floors{}); len(f) != 0 {
		t.Fatalf("no floors: %v", f)
	}
	if f := r.BelowFloors(Floors{AUC: 0.75, PrecisionAtK: 0.5, RecallPopular: 0.5}); len(f) != 0 {
		t.Fatalf("at its floors: %v", f)
	}
	f := r.BelowFloors(Floors{AUC: 0.76, PrecisionAtK: 0.51, RecallPopular: 0.51})
	want := []string{"eval: AUC 0.750 < floor 0.760", "eval: precision@k 0.500 < floor 0.510", "eval: recall of popular stories 0.500 < floor 0.510"}
	if strings.Join(f, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", f, want)
	}
	// With a floor set, an unscorable report fails instead of passing.
	if f := (&EvalReport{}).BelowFloors(Floors{AUC: 0.5}); len(f) != 1 || !strings.Contains(f[0], "no story has a popularity truth label") {
		t.Fatalf("no truth: %v", f)
	}
	if f := (&EvalReport{Scored: 3, Positive: 0}).BelowFloors(Floors{AUC: 0.5}); len(f) != 1 || !strings.Contains(f[0], "undefined") {
		t.Fatalf("no positives: %v", f)
	}
	if f := (&EvalReport{Scored: 3, Positive: 3}).BelowFloors(Floors{RecallPopular: 0.5}); len(f) != 1 || !strings.Contains(f[0], "undefined") {
		t.Fatalf("all positive: %v", f)
	}
}

func TestAUCPinned(t *testing.T) {
	// pos {0.9, 0.5}, neg {0.5, 0.1, 0.7}: 0.9 beats all 3; 0.5 ties 0.5
	// (0.5), beats 0.1, loses to 0.7. U = 3 + 1.5 = 4.5 of 6.
	got := AUC([]float64{0.9, 0.5, 0.5, 0.1, 0.7}, []bool{true, true, false, false, false})
	if got != 0.75 {
		t.Fatalf("AUC = %v, want 0.75", got)
	}
	if AUC([]float64{1, 2}, []bool{true, true}) != 0 || AUC(nil, nil) != 0 {
		t.Fatal("AUC with one class empty must be 0")
	}
}

// day ms: t0 is 2026-11-08 12:00 UTC.
var (
	day1 = ms(-36 * time.Hour) // 2026-11-07 00:00
	day2 = ms(-12 * time.Hour) // 2026-11-08 00:00
)

func ip(v int) *int { return &v }

// rankScoreFixture: two UTC days of one topic, P(post) pairwise distinct and
// all below the old 0.6 gate (which would have posted nothing), k = 2.
//
//	day 1: s_a 0.52 post, s_b 0.47 unknown, s_d 0.35 post, s_c 0.31 drop
//	day 2: s_e 0.44 digest, s_f 0.39 post, s_g 0.12 drop (excluded: hf only, truth post)
//
// Picks: day 1 s_a s_b, day 2 s_e s_f. Scored: a c d e f (b unknown, g
// excluded); popular a d f. Picked scored: a e f, of which a f are post:
// precision@k 2/3; recall 2/3 (d missed). AUC over a(.52,+) c(.31,-) d(.35,+)
// e(.44,-) f(.39,+): a beats both negatives (2), d and f beat c only (1 each):
// 4 of 6 = 0.667. (Over the picked stories alone it would be 0.5.)
func rankScoreFixture() ([]EvalRow, map[string]TruthRow) {
	row := func(story string, created int64, p float64) EvalRow {
		return EvalRow{Topic: "top-llm-papers", Story: story, Day: utcDate(created), URL: "https://arxiv.org/abs/2610.0000" + story[2:], PPost: p, Model: "m", created: created}
	}
	rows := []EvalRow{
		row("s_g", day2+3, 0.12), row("s_a", day1+1, 0.52), row("s_c", day1+3, 0.31), row("s_e", day2+1, 0.44),
		row("s_b", day1+2, 0.47), row("s_f", day2+2, 0.39), row("s_d", day1+4, 0.35),
	}
	tr := func(id, story, label, excl string) TruthRow {
		return TruthRow{ID: id, URL: "https://arxiv.org/abs/2610.0000" + story[2:], Signal: SignalHF, Value: ip(1), Label: label, Excluded: excl}
	}
	truth := map[string]TruthRow{
		"m_a": tr("m_a", "s_a", "post", ""), "m_c": tr("m_c", "s_c", "drop", ""), "m_d": tr("m_d", "s_d", "post", ""),
		"m_e": tr("m_e", "s_e", "digest", ""), "m_f": tr("m_f", "s_f", "post", ""), "m_g": tr("m_g", "s_g", "post", ExcludedHFDailyOnly),
		"m_b": {ID: "m_b", URL: "https://arxiv.org/abs/2610.0000b", Why: "not_on_hf"},
	}
	return rows, truth
}

type scoreSummary struct {
	Stories, Days, Picked, Scored, Unknown, Excluded, Positive, PickedScored, PickedPositive int
	PrecisionAtK, RecallPopular, AUC                                                         float64
	PickedStories                                                                            []string
}

func summarise(rep *EvalReport) scoreSummary {
	s := scoreSummary{Stories: rep.Stories, Days: rep.Days, Picked: rep.Picked, Scored: rep.Scored, Unknown: rep.Unknown,
		Excluded: rep.Excluded, Positive: rep.Positive, PickedScored: rep.PickedScored, PickedPositive: rep.PickedPositive,
		PrecisionAtK: math.Round(rep.PrecisionAtK*1e6) / 1e6, RecallPopular: math.Round(rep.RecallPopular*1e6) / 1e6, AUC: math.Round(rep.AUC*1e6) / 1e6,
		PickedStories: []string{}}
	for _, r := range rep.Rows {
		if r.Picked {
			s.PickedStories = append(s.PickedStories, r.Story)
		}
	}
	sort.Strings(s.PickedStories)
	return s
}

// TestEvalScoresTheRankPick pins eval's scoring of the top_k_per_day pick:
// picks per (topic, UTC day), unknown and excluded stories compete for the
// picks but are not scored, and the three scores.
func TestEvalScoresTheRankPick(t *testing.T) {
	rows, truth := rankScoreFixture()
	rep := &EvalReport{Rows: rows}
	scoreRows(rep, truthByURL(truth), DecideConfig{TopKPerDay: 2})
	want := scoreSummary{Stories: 7, Days: 2, Picked: 4, Scored: 5, Unknown: 1, Excluded: 1, Positive: 3, PickedScored: 3, PickedPositive: 2,
		PrecisionAtK: 0.666667, RecallPopular: 0.666667, AUC: 0.666667, PickedStories: []string{"s_a", "s_b", "s_e", "s_f"}}
	if got := summarise(rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	// k = 3: day 1 adds s_d (post), day 2 adds s_g (excluded): precision
	// 3/4, recall 3/3.
	rows, _ = rankScoreFixture()
	rep = &EvalReport{Rows: rows}
	scoreRows(rep, truthByURL(truth), DecideConfig{TopKPerDay: 3})
	if got := summarise(rep); got.PickedScored != 4 || got.PickedPositive != 3 || got.PrecisionAtK != 0.75 || got.RecallPopular != 1 ||
		!reflect.DeepEqual(got.PickedStories, []string{"s_a", "s_b", "s_d", "s_e", "s_f", "s_g"}) {
		t.Fatalf("k=3: %+v", got)
	}
	// The min_p_post floor: 0.45 leaves day 1's s_a and nothing on day 2.
	rows, _ = rankScoreFixture()
	rep = &EvalReport{Rows: rows}
	scoreRows(rep, truthByURL(truth), DecideConfig{TopKPerDay: 2, MinPPost: 0.45})
	if got := summarise(rep); !reflect.DeepEqual(got.PickedStories, []string{"s_a", "s_b"}) || got.PrecisionAtK != 1 || got.RecallPopular != 0.333333 {
		t.Fatalf("floor 0.45: %+v", got)
	}
}

func writePreview(t *testing.T, dir, date string, rows ...PreviewRow) {
	t.Helper()
	var b bytes.Buffer
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "preview-"+date+".jsonl"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func truthFile(t *testing.T, truth map[string]TruthRow) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "truth.json")
	if err := writeTruth(p, truth); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEvalReadsPreviewFiles: eval's item source can be run -preview's files:
// every story once (its first row), its day from its first item, no model
// call, scored against truth.json. The preview's own pick status is not
// used: eval picks again by the same rule.
func TestEvalReadsPreviewFiles(t *testing.T) {
	rows, truth := rankScoreFixture()
	dir := t.TempDir()
	prev := func(r EvalRow, status string) PreviewRow {
		return PreviewRow{Topic: r.Topic, Story: r.Story, URL: r.URL, Created: r.created, Status: status, Model: "perplexity/pplx-decider-v1.2",
			Probs: map[string]float64{"post": r.PPost, "digest": 0.1, "drop": 0.1}}
	}
	by := map[string]EvalRow{}
	for _, r := range rows {
		by[r.Story] = r
	}
	// File 1 (2026-11-07) holds day 1's stories; file 2 repeats two of them
	// (a later run's rows, here with a different P(post) the first row must
	// win over) and adds day 2's. Status words are deliberately wrong.
	writePreview(t, dir, "2026-11-07", prev(by["s_a"], "not_picked"), prev(by["s_b"], "not_picked"), prev(by["s_c"], "picked"), prev(by["s_d"], "picked"))
	late := prev(by["s_d"], "posted")
	late.Probs = map[string]float64{"post": 0.99}
	writePreview(t, dir, "2026-11-08", late, prev(by["s_a"], "posted"), prev(by["s_e"], "picked"), prev(by["s_f"], "picked"), prev(by["s_g"], "not_picked"))
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600) // not a preview file

	f := newRunFixture(t)
	f.cfg.Decide.TopKPerDay = 2
	o := CmdOpts{Config: f.cfg, Truth: truthFile(t, truth), PreviewDir: dir, HTTP: f.or.srv.Client(), Now: func() time.Time { return t0 }}
	t.Setenv("OPENROUTER_API_KEY", "") // no key: the preview source needs none
	var out bytes.Buffer
	if code := Command(context.Background(), "eval", o, &out); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if n := f.or.count("decisions"); n != 0 {
		t.Fatalf("preview eval made %d model call(s)", n)
	}
	want := "eval stories=7 days=2 picked=4 scored=5 unknown=1 excluded=1 popular=3 picked_scored=3 picked_popular=2 precision_at_k=0.667 recall_popular=0.667 auc=0.667"
	if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "eval source=preview:"+dir+" models=[perplexity/pplx-decider-v1.2] top_k_per_day=2") {
		t.Fatalf("output:\n%s\nwant %s", out.String(), want)
	}
	// No preview file: an error, not an empty pass.
	o.PreviewDir = t.TempDir()
	out.Reset()
	if code := Command(context.Background(), "eval", o, &out); code != 1 || !strings.Contains(out.String(), "no preview-*.jsonl files") {
		t.Fatalf("empty dir: exit %d\n%s", code, out.String())
	}
}

// TestEvalReadsHubWindow: without -preview-dir, eval reads the source items
// of the last -since-hours, groups them by canonical URL as run does (one
// decision per story), asks the decider, and scores the pick per UTC day.
func TestEvalReadsHubWindow(t *testing.T) {
	f := newRunFixture(t) // A (-3h), B (-2h), C (-1h): all 2026-11-08
	// Day 1 (2026-11-07): D, and the same paper as D again (pdf v2, another
	// item of D's story); E. Too old for a 48h window: F.
	rawPost(t, f.hub, f.curator, f.rawID, "Paper D", "https://arxiv.org/abs/2610.00004v1", "About agents D.", day1+10)
	rawPost(t, f.hub, f.curator, f.rawID, "Paper D again", "https://arxiv.org/pdf/2610.00004v2", "About agents D.", day1+20)
	rawPost(t, f.hub, f.curator, f.rawID, "Paper E", "https://arxiv.org/abs/2610.00005v1", "About agents E.", day1+30)
	rawPost(t, f.hub, f.curator, f.rawID, "Paper F", "https://arxiv.org/abs/2610.00006v1", "About agents F.", ms(-49*time.Hour))
	f.or.probs["Paper D"] = map[string]float64{"post": 0.21, "digest": 0.5, "drop": 0.29}
	f.or.probs["Paper E"] = map[string]float64{"post": 0.34, "digest": 0.5, "drop": 0.16}
	f.or.probs["Paper F"] = map[string]float64{"post": 0.99, "digest": 0, "drop": 0.01}
	// A 0.9, B 0.45, C 0.8 (the run fixture). k = 1: day 2 picks A, day 1 E.
	f.cfg.Decide.TopKPerDay = 1
	at := func(n int, label string) TruthRow {
		return TruthRow{ID: "m_" + label + string(rune('0'+n)), URL: "https://arxiv.org/abs/2610.0000" + string(rune('0'+n)), Signal: SignalHF, Value: ip(n), Label: label}
	}
	truth := map[string]TruthRow{}
	for _, r := range []TruthRow{at(1, "drop"), at(2, "post"), at(3, "post"), at(4, "post"), at(5, "drop"), at(6, "post")} {
		truth[r.ID] = r
	}
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key\n"), 0o600)
	t.Setenv("OPENROUTER_API_KEY", "")
	f.cfg.OpenRouterURL = f.or.srv.URL
	pred := filepath.Join(t.TempDir(), "pred.jsonl")
	o := CmdOpts{Config: f.cfg, KeyFile: key, Truth: truthFile(t, truth), SinceHours: 48, Predictions: pred, HTTP: f.hub.srv.Client(),
		Now: func() time.Time { return t0 }}
	var out bytes.Buffer
	if code := Command(context.Background(), "eval", o, &out); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	// Five stories (D's two items are one), F outside the window: 5 decisions.
	if n := f.or.count("decisions"); n != 5 {
		t.Fatalf("%d decisions, want 5 (one per story in the window)\n%s", n, out.String())
	}
	// Scored A(.9,-) B(.45,+) C(.8,+) D(.21,+) E(.34,-); picked A, E: none
	// post, precision 0/2, recall 0/3. AUC: B beats E (1), C beats E (1), D
	// beats neither: 2 of 6.
	want := "eval stories=5 days=2 picked=2 scored=5 unknown=0 excluded=0 popular=3 picked_scored=2 picked_popular=0 precision_at_k=0.000 recall_popular=0.000 auc=0.333"
	if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "eval source=hub:48h") || !strings.Contains(out.String(), "decisions=5 ") {
		t.Fatalf("output:\n%s\nwant %s", out.String(), want)
	}
	raw, _ := os.ReadFile(pred)
	var picked []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r EvalRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		if r.Picked {
			picked = append(picked, r.Day+" "+r.URL)
		}
	}
	if w := []string{"2026-11-07 https://arxiv.org/abs/2610.00005", "2026-11-08 https://arxiv.org/abs/2610.00001"}; !reflect.DeepEqual(picked, w) {
		t.Fatalf("picked %v, want %v", picked, w)
	}
}

// TestEvalAxis1SkipsExcludedRows: an excluded story is not scored, even when
// its truth is post, and is counted as excluded.
func TestEvalAxis1SkipsExcludedRows(t *testing.T) {
	row := func(story string, p float64) EvalRow {
		return EvalRow{Topic: "top-x", Story: story, Day: "2026-11-08", URL: "https://x.example/" + story, PPost: p, created: day2}
	}
	v := 50
	truth := map[string]TruthRow{
		"m_1": {ID: "m_1", URL: "https://x.example/s_hot", Signal: SignalHF, Value: &v, Label: "post"},
		"m_2": {ID: "m_2", URL: "https://x.example/s_cold", Signal: SignalHF, Value: &v, Label: "drop"},
		"m_3": {ID: "m_3", URL: "https://x.example/s_circ", Signal: SignalHF, Value: &v, Label: "post", Excluded: ExcludedHFDailyOnly},
	}
	rep := &EvalReport{Rows: []EvalRow{row("s_hot", 0.9), row("s_cold", 0.1), row("s_circ", 0.05)}}
	scoreRows(rep, truthByURL(truth), DecideConfig{TopKPerDay: 4})
	// Scored: hot (post, P 0.9) and cold (drop, P 0.1): AUC 1. With circ
	// (post, P 0.05, below cold) scored too, AUC would be 0.5 and popular 2.
	if rep.Scored != 2 || rep.Excluded != 1 || rep.Unknown != 0 || rep.Positive != 1 || rep.AUC != 1 {
		t.Fatalf("scored %d excluded %d unknown %d popular %d auc %.3f", rep.Scored, rep.Excluded, rep.Unknown, rep.Positive, rep.AUC)
	}
}
