package editor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// eval scores what run would POST, the top_k_per_day rank pick, against the
// popularity truth (truth.json, written by `editor truth`). It reads its
// stories from one of two places:
//
//   - the preview files (-preview-dir): every decided story run -preview
//     logged, with the P(post) it got then. No model is called, so this scores
//     the decider that wrote the files.
//   - the hub window (the default): every source item of the last
//     SinceHours, grouped into stories the way run groups them and decided by
//     the configured decider. This is how another decider is scored on the
//     same stories.
//
// Each topic's stories are split by the UTC day of their first item, and each
// day's are picked as run picks them (RankPick: the top_k_per_day highest
// P(post), ties by story id, at or above min_p_post). That approximates run,
// which also carries an unpicked story into the next days while it is inside
// decide.window_hours; eval gives each story one day only.

// EvalOptions configure eval.
type EvalOptions struct {
	Config *Config
	// Truth is the popularity truth by item id. Stories join it by
	// canonical URL. Without it nothing can be scored.
	Truth map[string]TruthRow
	// PreviewDir, when set, is the directory of run -preview's
	// preview-YYYY-MM-DD.jsonl files (<state>/preview).
	PreviewDir string
	// Hub and Model are used when PreviewDir is empty.
	Hub   *hubkit.Client
	Model *Model
	// SinceHours: the hub window reads items at most this old (default 336).
	SinceHours int
	Now        func() time.Time
	Log        io.Writer
	// Predictions, if set, receives one JSON line per story (EvalRow).
	Predictions io.Writer
}

// EvalRow is one decided story of one topic, as eval scored it.
type EvalRow struct {
	Topic    string  `json:"topic"`
	Story    string  `json:"story"`
	Day      string  `json:"day"` // UTC date of the story's first item
	URL      string  `json:"url"`
	PPost    float64 `json:"p_post"`
	Model    string  `json:"model"`
	Picked   bool    `json:"picked"`
	Truth    string  `json:"truth"` // post | digest | drop, "" unknown
	Excluded string  `json:"excluded,omitempty"`
	Signal   string  `json:"signal,omitempty"`
	Value    *int    `json:"value,omitempty"`
	created  int64
}

// EvalReport is the result of eval.
type EvalReport struct {
	Source   string
	Models   []string // the decision models whose P(post) was scored
	K        int
	MinPPost float64
	Rows     []EvalRow

	Stories  int // decided stories, all topics
	Days     int // distinct (topic, UTC day) groups
	Picked   int
	Scored   int // stories with a truth label (not excluded)
	Unknown  int // stories whose truth is unknown
	Excluded int // stories whose truth is excluded (hf_daily_only)
	Positive int // scored stories whose truth is post (the popular ones)
	// PickedScored: picked stories with a truth label; PickedPositive: picked
	// stories whose truth is post.
	PickedScored, PickedPositive int
	// PrecisionAtK = PickedPositive / PickedScored; RecallPopular =
	// PickedPositive / Positive; AUC of P(post) over the scored stories,
	// truth post against truth digest/drop.
	PrecisionAtK, RecallPopular, AUC float64

	// Decision cost and latency (hub window only).
	Decisions   int
	DecideSpend float64
	Latencies   []time.Duration
}

// AUC is the probability that a random positive scores above a random
// negative (Mann-Whitney U / (n+ * n-)), ties counting half. It is 0 when
// either class is empty. It is pure.
func AUC(scores []float64, positive []bool) float64 {
	var pos, neg []float64
	for i, s := range scores {
		if positive[i] {
			pos = append(pos, s)
		} else {
			neg = append(neg, s)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return 0
	}
	u := 0.0
	for _, p := range pos {
		for _, n := range neg {
			switch {
			case p > n:
				u++
			case p == n:
				u += 0.5
			}
		}
	}
	return u / float64(len(pos)*len(neg))
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Floors are eval's exit floors.
type Floors struct {
	AUC           float64
	PrecisionAtK  float64
	RecallPopular float64
}

func (f Floors) any() bool { return f.AUC > 0 || f.PrecisionAtK > 0 || f.RecallPopular > 0 }

// BelowFloors lists every floor the report misses. With any floor set, a
// report that cannot be scored (no truth, or no popular story, or nothing
// else) is a failure, not a pass.
func (r *EvalReport) BelowFloors(f Floors) []string {
	if !f.any() {
		return nil
	}
	if r == nil || r.Scored == 0 {
		return []string{"eval: no story has a popularity truth label (pass -truth)"}
	}
	if r.Positive == 0 || r.Positive == r.Scored {
		return []string{fmt.Sprintf("eval: %d of %d scored stories are post: AUC, precision and recall are undefined", r.Positive, r.Scored)}
	}
	var out []string
	if r.AUC < f.AUC {
		out = append(out, fmt.Sprintf("eval: AUC %.3f < floor %.3f", r.AUC, f.AUC))
	}
	if r.PrecisionAtK < f.PrecisionAtK {
		out = append(out, fmt.Sprintf("eval: precision@k %.3f < floor %.3f", r.PrecisionAtK, f.PrecisionAtK))
	}
	if r.RecallPopular < f.RecallPopular {
		out = append(out, fmt.Sprintf("eval: recall of popular stories %.3f < floor %.3f", r.RecallPopular, f.RecallPopular))
	}
	return out
}

// Write prints the report.
func (r *EvalReport) Write(w io.Writer) {
	fmt.Fprintf(w, "eval source=%s models=%v top_k_per_day=%d min_p_post=%.3f\n", r.Source, r.Models, r.K, r.MinPPost)
	if r.Decisions > 0 {
		lat := append([]time.Duration(nil), r.Latencies...)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		var sum time.Duration
		for _, l := range lat {
			sum += l
		}
		fmt.Fprintf(w, "decisions=%d spend=$%.6f cost_per_100=$%.6f latency_mean=%dms p50=%dms p95=%dms\n", r.Decisions, r.DecideSpend,
			r.DecideSpend*100/float64(r.Decisions), (sum / time.Duration(len(lat))).Milliseconds(), lat[len(lat)/2].Milliseconds(), lat[min(len(lat)-1, len(lat)*95/100)].Milliseconds())
	}
	fmt.Fprintf(w, "eval stories=%d days=%d picked=%d scored=%d unknown=%d excluded=%d popular=%d picked_scored=%d picked_popular=%d precision_at_k=%.3f recall_popular=%.3f auc=%.3f\n",
		r.Stories, r.Days, r.Picked, r.Scored, r.Unknown, r.Excluded, r.Positive, r.PickedScored, r.PickedPositive, r.PrecisionAtK, r.RecallPopular, r.AUC)
}

// storyTruth is a story's truth, joined by canonical URL: excluded if any row
// for the URL is, else the label of the first labelled row (by item id).
type storyTruth struct {
	label, excluded, signal string
	value                   *int
}

func truthByURL(rows map[string]TruthRow) map[string]storyTruth {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := map[string]storyTruth{}
	for _, id := range ids {
		r := rows[id]
		if r.URL == "" {
			continue
		}
		st := out[r.URL]
		if r.Excluded != "" {
			st.excluded = r.Excluded
		}
		if st.label == "" && r.Label != "" {
			st.label, st.signal, st.value = r.Label, r.Signal, r.Value
		}
		out[r.URL] = st
	}
	return out
}

// Eval scores the rank pick against the truth.
func Eval(ctx context.Context, o EvalOptions) (*EvalReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.SinceHours == 0 {
		o.SinceHours = 336
	}
	cfg := o.Config
	rep := &EvalReport{K: cfg.Decide.TopKPerDay, MinPPost: cfg.Decide.MinPPost}
	var err error
	if o.PreviewDir != "" {
		rep.Source = "preview:" + o.PreviewDir
		rep.Rows, err = previewRows(o.PreviewDir)
	} else {
		rep.Source = fmt.Sprintf("hub:%dh", o.SinceHours)
		rep.Rows, err = hubRows(ctx, o, rep)
	}
	if err != nil {
		return nil, err
	}
	scoreRows(rep, truthByURL(o.Truth), cfg.Decide)
	if o.Predictions != nil {
		for _, row := range rep.Rows {
			raw, _ := json.Marshal(row)
			o.Predictions.Write(append(raw, '\n'))
		}
	}
	return rep, nil
}

// scoreRows joins the truth, picks each (topic, UTC day) and fills the
// report's counts. It is pure.
func scoreRows(rep *EvalReport, truth map[string]storyTruth, dc DecideConfig) {
	sort.Slice(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if a.Topic != b.Topic {
			return a.Topic < b.Topic
		}
		if a.created != b.created {
			return a.created < b.created
		}
		return a.Story < b.Story
	})
	groups := map[string][]Candidate{}
	var order []string
	models := map[string]bool{}
	for i := range rep.Rows {
		row := &rep.Rows[i]
		models[row.Model] = true
		if t, ok := truth[row.URL]; ok {
			row.Truth, row.Excluded, row.Signal, row.Value = t.label, t.excluded, t.signal, t.value
		}
		g := row.Topic + " " + row.Day
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], Candidate{Story: row.Story, PPost: row.PPost})
	}
	picked := map[string]bool{}
	for _, g := range order {
		topic, _, _ := strings.Cut(g, " ")
		for _, c := range RankPick(groups[g], dc.TopKPerDay, dc.MinPPost) {
			picked[topic+" "+c.Story] = true
		}
	}
	var scores []float64
	var pos []bool
	for i := range rep.Rows {
		row := &rep.Rows[i]
		row.Picked = picked[row.Topic+" "+row.Story]
		rep.Stories++
		if row.Picked {
			rep.Picked++
		}
		switch {
		case row.Excluded != "":
			// Graded by its own source's votes (circular): not scored.
			rep.Excluded++
			continue
		case row.Truth == "":
			rep.Unknown++
			continue
		}
		rep.Scored++
		scores = append(scores, row.PPost)
		pos = append(pos, row.Truth == "post")
		if row.Truth == "post" {
			rep.Positive++
		}
		if row.Picked {
			rep.PickedScored++
			if row.Truth == "post" {
				rep.PickedPositive++
			}
		}
	}
	rep.Days = len(order)
	rep.PrecisionAtK = ratio(rep.PickedPositive, rep.PickedScored)
	rep.RecallPopular = ratio(rep.PickedPositive, rep.Positive)
	rep.AUC = AUC(scores, pos)
	for m := range models {
		rep.Models = append(rep.Models, m)
	}
	sort.Strings(rep.Models)
}

func utcDate(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

// previewRows reads every preview-*.jsonl file in dir, oldest file first. A
// story's first row gives its P(post): a story is decided once, so later
// rows repeat it.
func previewRows(dir string) ([]EvalRow, error) {
	files, err := filepath.Glob(filepath.Join(dir, "preview-*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("eval: no preview-*.jsonl files in %s", dir)
	}
	sort.Strings(files)
	seen := map[string]bool{}
	var rows []EvalRow
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for n := 1; sc.Scan(); n++ {
			if len(strings.TrimSpace(sc.Text())) == 0 {
				continue
			}
			var p PreviewRow
			if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
				fh.Close()
				return nil, fmt.Errorf("%s:%d: %w", f, n, err)
			}
			pp, ok := p.Probs["post"]
			if p.Topic == "" || p.Story == "" || !ok {
				fh.Close()
				return nil, fmt.Errorf("%s:%d: want topic, story and probs.post", f, n)
			}
			if seen[p.Topic+" "+p.Story] {
				continue
			}
			seen[p.Topic+" "+p.Story] = true
			rows = append(rows, EvalRow{Topic: p.Topic, Story: p.Story, Day: utcDate(p.Created), URL: p.URL, PPost: pp, Model: p.Model, created: p.Created})
		}
		err = sc.Err()
		fh.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return rows, nil
}

// hubRows reads the source items of the window, groups them as run does and
// asks the decider about every story of every topic.
func hubRows(ctx context.Context, o EvalOptions, rep *EvalReport) ([]EvalRow, error) {
	if o.Hub == nil || o.Model == nil {
		return nil, fmt.Errorf("eval: the hub window needs a hub and a model (or pass -preview-dir)")
	}
	cfg := o.Config
	items, err := ReadSources(ctx, o.Hub, cfg, o.Log)
	if err != nil {
		return nil, err
	}
	cutoff := o.Now().UnixMilli() - int64(o.SinceHours)*3600_000
	var recent []Item
	for _, it := range items {
		if it.TS >= cutoff {
			recent = append(recent, it)
		}
	}
	st := newState()
	(&Clusterer{Cfg: cfg.Cluster, State: st}).Assign(recent)
	ids := make([]string, 0, len(st.Stories))
	for id := range st.Stories {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := st.Stories[ids[i]], st.Stories[ids[j]]
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.ID < b.ID
	})
	var rows []EvalRow
	for i := range cfg.Topics {
		t := &cfg.Topics[i]
		for _, id := range ids {
			s := st.Stories[id]
			if !inTopic(t, s) {
				continue
			}
			before, start := o.Model.Meter.Spent(), time.Now()
			d, err := DecideStory(ctx, o.Model, cfg.DecisionModel, cfg.Decide, t, s, o.Now().UnixMilli())
			if err != nil {
				return nil, fmt.Errorf("topic %s story %s: %w", t.Slug, id, err)
			}
			rep.Latencies = append(rep.Latencies, time.Since(start))
			rep.DecideSpend += o.Model.Meter.Spent() - before
			rep.Decisions++
			rows = append(rows, EvalRow{Topic: t.Slug, Story: id, Day: utcDate(s.Created), URL: storyURL(s), PPost: d.Probs["post"], Model: d.Model, created: s.Created})
		}
	}
	return rows, nil
}
