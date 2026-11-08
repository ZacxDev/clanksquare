package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
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
	// Predictions, if set, receives one JSON line per scored item row:
	// {"id","truth","pred","probs"}, for sweeping thresholds offline.
	Predictions io.Writer
}

// EvalReport is the result of eval.
type EvalReport struct {
	Items      []PRF
	Pairs      PRF // the "same" label over pairs: pairwise cluster F1
	ItemRows   int
	PairRows   int
	Thresholds string
	Confusion  map[string]map[string]int // truth -> pred -> n
}

// BelowFloors lists every floor the report misses. A label with no support
// has no F1 and is not checked.
func (r *EvalReport) BelowFloors(floorF1, floorPairF1 float64) []string {
	var out []string
	for _, p := range r.Items {
		if p.Support > 0 && p.F1() < floorF1 {
			out = append(out, fmt.Sprintf("label %s F1 %.3f < floor %.3f", p.Label, p.F1(), floorF1))
		}
	}
	if r.PairRows > 0 && r.Pairs.F1() < floorPairF1 {
		out = append(out, fmt.Sprintf("pairwise cluster F1 %.3f < floor %.3f", r.Pairs.F1(), floorPairF1))
	}
	return out
}

// Write prints the report.
func (r *EvalReport) Write(w io.Writer) {
	fmt.Fprintf(w, "eval %s item_rows=%d pair_rows=%d\n", r.Thresholds, r.ItemRows, r.PairRows)
	for _, p := range r.Items {
		fmt.Fprintf(w, "label=%s support=%d precision=%.3f recall=%.3f f1=%.3f\n", p.Label, p.Support, p.Precision(), p.Recall(), p.F1())
	}
	truths := make([]string, 0, len(r.Confusion))
	for t := range r.Confusion {
		truths = append(truths, t)
	}
	sort.Strings(truths)
	for _, t := range truths {
		fmt.Fprintf(w, "confusion truth=%s", t)
		for _, l := range Labels {
			fmt.Fprintf(w, " %s=%d", l, r.Confusion[t][l])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "pairs support=%d precision=%.3f recall=%.3f pairwise_cluster_f1=%.3f\n", r.Pairs.Support, r.Pairs.Precision(), r.Pairs.Recall(), r.Pairs.F1())
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
		Thresholds: fmt.Sprintf("decision_model=%s post>=%.2f digest>=%.2f embed_model=%s cluster>=%.2f", cfg.DecisionModel, cfg.Decide.Post, cfg.Decide.Digest, cfg.EmbedModel, cfg.Cluster.Threshold)}
	var itemTruth, itemPred, pairTruth, pairPred []string
	var pairRows []LabelRow
	for _, r := range rows {
		t := r.Truth(o.HumanOnly)
		if t == "" {
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
			d, err := DecideStory(ctx, o.Model, cfg.DecisionModel, cfg.Decide, topic, st, 0)
			if err != nil {
				return nil, fmt.Errorf("label %s: %w", r.ID, err)
			}
			itemTruth = append(itemTruth, t)
			itemPred = append(itemPred, d.Label)
			if o.Predictions != nil {
				raw, _ := json.Marshal(map[string]any{"id": r.ID, "truth": t, "pred": d.Label, "probs": d.Probs})
				o.Predictions.Write(append(raw, '\n'))
			}
			if rep.Confusion[t] == nil {
				rep.Confusion[t] = map[string]int{}
			}
			rep.Confusion[t][d.Label]++
		case "pair":
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
