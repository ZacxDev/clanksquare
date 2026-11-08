package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// PRF is precision, recall and F1 for one label.
type PRF struct {
	Label      string
	TP, FP, FN int
	Support    int // rows whose truth is this label
}

func (p PRF) Precision() float64 { return ratio(p.TP, p.TP+p.FP) }
func (p PRF) Recall() float64    { return ratio(p.TP, p.TP+p.FN) }
func (p PRF) F1() float64 {
	pr, rc := p.Precision(), p.Recall()
	if pr+rc == 0 {
		return 0
	}
	return 2 * pr * rc / (pr + rc)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Score computes per-label precision, recall and F1 of pred against truth.
// Rows with an empty truth are skipped. It is pure.
func Score(truth, pred []string, labels []string) []PRF {
	out := make([]PRF, len(labels))
	idx := map[string]int{}
	for i, l := range labels {
		out[i].Label = l
		idx[l] = i
	}
	for k := range truth {
		t, p := truth[k], pred[k]
		if t == "" {
			continue
		}
		if i, ok := idx[t]; ok {
			out[i].Support++
		}
		if t == p {
			if i, ok := idx[t]; ok {
				out[i].TP++
			}
			continue
		}
		if i, ok := idx[t]; ok {
			out[i].FN++
		}
		if i, ok := idx[p]; ok {
			out[i].FP++
		}
	}
	return out
}

// EvalOptions configure eval.
type EvalOptions struct {
	Config    *Config
	Model     *Model
	Labels    string
	HumanOnly bool
	// Truth is the popularity truth by item id (axis 1). Without it only
	// axis 2 (agreement with the labels file) is reported.
	Truth map[string]TruthRow
	// Predictions, if set, receives one JSON line per scored item row:
	// {"id","truth","pred","probs"}, for sweeping thresholds offline.
	Predictions io.Writer
}

// Axis1 scores the decisions against the popularity truth: the only axis
// eval gates on.
type Axis1 struct {
	Rows     int   // item rows with a truth label
	Unknown  int   // item rows whose truth is unknown (not scored)
	Items    []PRF // per label, against the truth
	Positive int   // rows whose truth is post (the popular items)
	// PrecisionPost: of the rows decided post, the share whose truth is post.
	// RecallPopular: of the rows whose truth is post, the share decided post.
	PrecisionPost, RecallPopular float64
	PredictedPost                int
	// AUC of P(post) separating truth post from truth digest/drop (ties
	// count half); NaN-free: 0 when either class is empty.
	AUC       float64
	Confusion map[string]map[string]int
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

// EvalReport is the result of eval.
type EvalReport struct {
	// Axis1 is nil without a truth file.
	Axis1 *Axis1
	// Axis 2: agreement with the labels file's model (or human) labels.
	// This is agreement with a model, not truth.
	Items      []PRF
	Pairs      PRF // the "same" label over pairs: pairwise cluster F1
	ItemRows   int
	PairRows   int
	LabelModel string
	Thresholds string
	Confusion  map[string]map[string]int // label -> pred -> n
	// Decision cost and latency, over the item decisions.
	Decisions   int
	DecideSpend float64
	Latencies   []time.Duration
}

// Floors are eval's exit floors. They apply to axis 1 (the popularity
// truth) only: agreement with a model's labels never fails eval.
type Floors struct {
	F1            float64 // every truth label with support
	AUC           float64
	PrecisionPost float64
	RecallPopular float64
}

func (f Floors) any() bool {
	return f.F1 > 0 || f.AUC > 0 || f.PrecisionPost > 0 || f.RecallPopular > 0
}

// BelowFloors lists every floor the report misses on axis 1. With any floor
// set, an axis 1 that cannot be scored (no truth, or no popular item) is a
// failure, not a pass. A label with no support has no F1 and is not checked.
func (r *EvalReport) BelowFloors(f Floors) []string {
	if !f.any() {
		return nil
	}
	a := r.Axis1
	if a == nil || a.Rows == 0 {
		return []string{"axis 1: no item has a popularity truth label (pass -truth)"}
	}
	if a.Positive == 0 || a.Positive == a.Rows {
		return []string{fmt.Sprintf("axis 1: %d of %d truth rows are post: AUC, precision and recall are undefined", a.Positive, a.Rows)}
	}
	var out []string
	for _, p := range a.Items {
		if p.Support > 0 && p.F1() < f.F1 {
			out = append(out, fmt.Sprintf("axis 1: label %s F1 %.3f < floor %.3f", p.Label, p.F1(), f.F1))
		}
	}
	if a.AUC < f.AUC {
		out = append(out, fmt.Sprintf("axis 1: AUC %.3f < floor %.3f", a.AUC, f.AUC))
	}
	if a.PrecisionPost < f.PrecisionPost {
		out = append(out, fmt.Sprintf("axis 1: precision@post %.3f < floor %.3f", a.PrecisionPost, f.PrecisionPost))
	}
	if a.RecallPopular < f.RecallPopular {
		out = append(out, fmt.Sprintf("axis 1: recall of popular items %.3f < floor %.3f", a.RecallPopular, f.RecallPopular))
	}
	return out
}

func writeConfusion(w io.Writer, prefix string, c map[string]map[string]int) {
	truths := make([]string, 0, len(c))
	for t := range c {
		truths = append(truths, t)
	}
	sort.Strings(truths)
	for _, t := range truths {
		fmt.Fprintf(w, "%s truth=%s", prefix, t)
		for _, l := range Labels {
			fmt.Fprintf(w, " %s=%d", l, c[t][l])
		}
		fmt.Fprintln(w)
	}
}

// Write prints the report.
func (r *EvalReport) Write(w io.Writer) {
	fmt.Fprintf(w, "eval %s item_rows=%d pair_rows=%d\n", r.Thresholds, r.ItemRows, r.PairRows)
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
	if a := r.Axis1; a != nil {
		fmt.Fprintf(w, "axis1 (popularity truth) rows=%d unknown=%d popular=%d predicted_post=%d auc=%.3f precision_at_post=%.3f recall_popular=%.3f\n",
			a.Rows, a.Unknown, a.Positive, a.PredictedPost, a.AUC, a.PrecisionPost, a.RecallPopular)
		for _, p := range a.Items {
			fmt.Fprintf(w, "axis1 label=%s support=%d precision=%.3f recall=%.3f f1=%.3f\n", p.Label, p.Support, p.Precision(), p.Recall(), p.F1())
		}
		writeConfusion(w, "axis1 confusion", a.Confusion)
	} else {
		fmt.Fprintln(w, "axis1 (popularity truth): not scored, no -truth")
	}
	fmt.Fprintf(w, "axis2 (agreement with a model, not truth: labels by %s)\n", r.LabelModel)
	for _, p := range r.Items {
		fmt.Fprintf(w, "axis2 label=%s support=%d precision=%.3f recall=%.3f f1=%.3f\n", p.Label, p.Support, p.Precision(), p.Recall(), p.F1())
	}
	writeConfusion(w, "axis2 confusion", r.Confusion)
	fmt.Fprintf(w, "axis2 pairs support=%d precision=%.3f recall=%.3f pairwise_cluster_f1=%.3f\n", r.Pairs.Support, r.Pairs.Precision(), r.Pairs.Recall(), r.Pairs.F1())
}

// Eval scores the configured decision model, decision thresholds and
// clustering rule against a labels file.
func Eval(ctx context.Context, o EvalOptions) (*EvalReport, error) {
	rows, err := ReadLabels(o.Labels)
	if err != nil {
		return nil, err
	}
	cfg := o.Config
	rep := &EvalReport{Confusion: map[string]map[string]int{},
		Thresholds: fmt.Sprintf("decision_model=%s api=%s question=%s post>=%.2f digest>=%.2f embed_model=%s cluster>=%.2f", cfg.DecisionModel, cfg.Decide.API, cfg.Decide.Question, cfg.Decide.Post, cfg.Decide.Digest, cfg.EmbedModel, cfg.Cluster.Threshold)}
	var a1 *Axis1
	var a1Truth, a1Pred []string
	var a1Score []float64
	var a1Pos []bool
	if o.Truth != nil {
		a1 = &Axis1{Confusion: map[string]map[string]int{}}
	}
	var itemTruth, itemPred, pairTruth, pairPred []string
	var pairRows []LabelRow
	models := map[string]bool{}
	for _, r := range rows {
		t := r.Truth(o.HumanOnly)
		pop := ""
		if r.Kind == "item" && o.Truth != nil {
			pop = o.Truth[r.ID].Label
			if pop == "" {
				a1.Unknown++
			}
		}
		if t == "" && pop == "" {
			continue
		}
		switch r.Kind {
		case "item":
			topic := topicFor(cfg, r.Channel)
			if topic == nil {
				return nil, fmt.Errorf("label %s: no topic reads channel %q", r.ID, r.Channel)
			}
			link := ""
			if len(r.Links) > 0 {
				link = r.Links[0]
			}
			st := &Story{ID: storyID(r.ID), Items: []StoryItem{{ID: r.ID, Channel: r.Channel, Title: r.Title, Excerpt: r.Excerpt, Link: link}}}
			before, start := o.Model.Meter.Spent(), time.Now()
			d, err := DecideStory(ctx, o.Model, cfg.DecisionModel, cfg.Decide, topic, st, 0)
			if err != nil {
				return nil, fmt.Errorf("label %s: %w", r.ID, err)
			}
			rep.Latencies = append(rep.Latencies, time.Since(start))
			rep.DecideSpend += o.Model.Meter.Spent() - before
			rep.Decisions++
			if pop != "" {
				a1Truth = append(a1Truth, pop)
				a1Pred = append(a1Pred, d.Label)
				a1Score = append(a1Score, d.Probs["post"])
				a1Pos = append(a1Pos, pop == "post")
				if a1.Confusion[pop] == nil {
					a1.Confusion[pop] = map[string]int{}
				}
				a1.Confusion[pop][d.Label]++
			}
			if o.Predictions != nil {
				row := map[string]any{"id": r.ID, "truth": t, "popularity": pop, "pred": d.Label, "probs": d.Probs}
				if tr, ok := o.Truth[r.ID]; ok && tr.Value != nil {
					row["signal"], row["value"] = tr.Signal, *tr.Value
				}
				raw, _ := json.Marshal(row)
				o.Predictions.Write(append(raw, '\n'))
			}
			if t == "" {
				continue
			}
			if r.Model != "" {
				models[r.Model] = true
			}
			itemTruth = append(itemTruth, t)
			itemPred = append(itemPred, d.Label)
			if rep.Confusion[t] == nil {
				rep.Confusion[t] = map[string]int{}
			}
			rep.Confusion[t][d.Label]++
		case "pair":
			if r.Model != "" {
				models[r.Model] = true
			}
			pairRows = append(pairRows, r)
			pairTruth = append(pairTruth, t)
		}
	}
	if len(pairRows) > 0 {
		preds, err := predictPairs(ctx, o.Model, cfg, pairRows)
		if err != nil {
			return nil, err
		}
		pairPred = preds
	}
	rep.ItemRows, rep.PairRows = len(itemTruth), len(pairTruth)
	rep.Items = Score(itemTruth, itemPred, Labels)
	rep.Pairs = Score(pairTruth, pairPred, []string{"same", "different"})[0]
	var ms []string
	for m := range models {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	rep.LabelModel = fmt.Sprint(ms)
	if a1 != nil {
		a1.Rows = len(a1Truth)
		a1.Items = Score(a1Truth, a1Pred, Labels)
		post := a1.Items[0] // Labels[0] is post
		a1.Positive = post.Support
		a1.PredictedPost = post.TP + post.FP
		a1.PrecisionPost, a1.RecallPopular = post.Precision(), post.Recall()
		a1.AUC = AUC(a1Score, a1Pos)
		rep.Axis1 = a1
	}
	return rep, nil
}

func topicFor(cfg *Config, channel string) *Topic {
	for i := range cfg.Topics {
		if contains(cfg.Topics[i].From, channel) {
			return &cfg.Topics[i]
		}
	}
	return nil
}

// predictPairs applies the clustering rule to each pair: the same canonical
// URL, or an embedding cosine JoinRule accepts (asking the decision model in
// the borderline band when that is on).
func predictPairs(ctx context.Context, m *Model, cfg *Config, rows []LabelRow) ([]string, error) {
	var texts []string
	for _, r := range rows {
		texts = append(texts, clipText(r.A.Title+"\n"+r.A.Excerpt, maxEmbedText), clipText(r.B.Title+"\n"+r.B.Excerpt, maxEmbedText))
	}
	var vecs [][]float64
	for start := 0; start < len(texts); start += embedBatch {
		embs, err := m.Embed(ctx, cfg.EmbedModel, texts[start:min(start+embedBatch, len(texts))])
		if err != nil {
			return nil, err
		}
		for _, e := range embs {
			vecs = append(vecs, unit(e))
		}
	}
	cl := &Clusterer{Cfg: cfg.Cluster, Model: m, DecisionModel: cfg.DecisionModel}
	out := make([]string, len(rows))
	for i, r := range rows {
		ka, kb := canonOf(r.A.Link), canonOf(r.B.Link)
		same := ka != "" && ka == kb
		if !same {
			switch JoinRule(cosine(vecs[2*i], vecs[2*i+1]), cfg.Cluster) {
			case JoinYes:
				same = true
			case JoinAsk:
				st := &Story{Items: []StoryItem{{Title: r.A.Title, Excerpt: r.A.Excerpt, Link: r.A.Link}}}
				ok, err := cl.sameStory(ctx, st, Item{Title: r.B.Title, Excerpt: r.B.Excerpt, Target: r.B.Link})
				if err != nil {
					return nil, err
				}
				same = ok
			}
		}
		if same {
			out[i] = "same"
		} else {
			out[i] = "different"
		}
	}
	return out, nil
}
