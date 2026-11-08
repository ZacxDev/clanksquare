package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScorePinned(t *testing.T) {
	truth := []string{"post", "post", "digest", "drop", "drop", "drop", ""}
	pred := []string{"post", "drop", "post", "drop", "drop", "digest", "post"}
	got := Score(truth, pred, Labels)
	// post: TP1 FP1 FN1; digest: TP0 FP1 FN1; drop: TP2 FP1 FN1. The row
	// with no truth is skipped.
	want := []PRF{
		{Label: "post", TP: 1, FP: 1, FN: 1, Support: 2},
		{Label: "digest", TP: 0, FP: 1, FN: 1, Support: 1},
		{Label: "drop", TP: 2, FP: 1, FN: 1, Support: 3},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label %s: got %+v want %+v", want[i].Label, got[i], want[i])
		}
	}
	if p := got[0]; p.Precision() != 0.5 || p.Recall() != 0.5 || p.F1() != 0.5 {
		t.Errorf("post P/R/F1 = %v %v %v", p.Precision(), p.Recall(), p.F1())
	}
	if f := got[2].F1(); math.Abs(f-2.0/3) > 1e-12 {
		t.Errorf("drop F1 = %v, want 2/3", f)
	}
	if f := got[1].F1(); f != 0 {
		t.Errorf("digest F1 = %v, want 0", f)
	}
}

func TestBelowFloorsAxis1Only(t *testing.T) {
	// Axis 2 (agreement with model labels) is all wrong; axis 1 is mixed.
	a1 := &Axis1{Rows: 4, Positive: 2, Items: Score([]string{"post", "post", "drop", "drop"}, []string{"post", "drop", "drop", "post"}, Labels),
		AUC: 0.75, PrecisionPost: 0.5, RecallPopular: 0.5}
	r := &EvalReport{Axis1: a1, Items: Score([]string{"post", "drop"}, []string{"drop", "post"}, Labels),
		Pairs: Score([]string{"same"}, []string{"different"}, []string{"same", "different"})[0], PairRows: 1}
	if f := r.BelowFloors(Floors{}); len(f) != 0 {
		t.Fatalf("no floors: %v", f)
	}
	// Floors axis 1 meets pass, however bad axis 2 is.
	if f := r.BelowFloors(Floors{F1: 0.5, AUC: 0.75, PrecisionPost: 0.5, RecallPopular: 0.5}); len(f) != 0 {
		t.Fatalf("axis 1 at its floors: %v", f)
	}
	f := r.BelowFloors(Floors{AUC: 0.76, PrecisionPost: 0.51, RecallPopular: 0.51, F1: 0.51})
	want := []string{"axis 1: label post F1 0.500 < floor 0.510", "axis 1: label drop F1 0.500 < floor 0.510",
		"axis 1: AUC 0.750 < floor 0.760", "axis 1: precision@post 0.500 < floor 0.510", "axis 1: recall of popular items 0.500 < floor 0.510"}
	if strings.Join(f, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", f, want)
	}
	// With a floor set, an unscorable axis 1 fails instead of passing.
	if f := (&EvalReport{}).BelowFloors(Floors{AUC: 0.5}); len(f) != 1 || !strings.Contains(f[0], "no item has a popularity truth label") {
		t.Fatalf("no truth: %v", f)
	}
	if f := (&EvalReport{Axis1: &Axis1{Rows: 3, Positive: 0}}).BelowFloors(Floors{AUC: 0.5}); len(f) != 1 || !strings.Contains(f[0], "undefined") {
		t.Fatalf("no positives: %v", f)
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

func writeLabels(t *testing.T, rows ...LabelRow) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	var b bytes.Buffer
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		b.Write(raw)
		b.WriteByte('\n')
	}
	os.WriteFile(p, b.Bytes(), 0o600)
	return p
}

func strp(s string) *string { return &s }

func TestEvalAgainstFakeModels(t *testing.T) {
	or := newFakeOR(t)
	or.probs["Good paper"] = map[string]float64{"post": 0.9, "digest": 0.05, "drop": 0.05}
	or.probs["Meh paper"] = map[string]float64{"post": 0.5, "digest": 0.3, "drop": 0.2} // gated to drop
	or.vecs["A"] = unitVec(1, 0)
	or.vecs["B"] = unitVec(1, 0.1)
	or.vecs["C"] = unitVec(0, 1)
	cfg := &Config{HubURL: "https://hub.example", Sources: []string{"llm-papers"},
		Topics: []Topic{{Slug: "top-llm-papers", Title: "T", Describe: "d", From: []string{"llm-papers"}, About: "model operator"}}}
	cfg.Defaults()
	labels := writeLabels(t,
		LabelRow{ID: "m_1", Kind: "item", Channel: "llm-papers", Title: "Good paper", ModelLabel: "post"},
		LabelRow{ID: "m_2", Kind: "item", Channel: "llm-papers", Title: "Meh paper", ModelLabel: "post", HumanLabel: strp("digest")},
		LabelRow{ID: "m_3+m_4", Kind: "pair", A: &PairSide{Title: "A", Link: "https://a.example"}, B: &PairSide{Title: "B", Link: "https://b.example"}, ModelLabel: "same"},
		LabelRow{ID: "m_3+m_5", Kind: "pair", A: &PairSide{Title: "A", Link: "https://a.example"}, B: &PairSide{Title: "C", Link: "https://c.example"}, ModelLabel: "same"},
	)
	rep, err := Eval(context.Background(), EvalOptions{Config: cfg, Model: or.model(1), Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	// Items: post->post (TP), digest->drop. Pairs: A~B cosine 0.995 same, A~C 0 different.
	if rep.ItemRows != 2 || rep.Items[0].TP != 1 || rep.Items[1].FN != 1 || rep.Items[2].FP != 1 {
		t.Fatalf("items: %+v", rep.Items)
	}
	if rep.PairRows != 2 || rep.Pairs.TP != 1 || rep.Pairs.FN != 1 || rep.Pairs.FP != 0 {
		t.Fatalf("pairs: %+v", rep.Pairs)
	}
	// human-only scores only the reviewed row.
	rep, err = Eval(context.Background(), EvalOptions{Config: cfg, Model: or.model(1), Labels: labels, HumanOnly: true})
	if err != nil || rep.ItemRows != 1 || rep.PairRows != 0 {
		t.Fatalf("human-only: %v %+v", err, rep)
	}
}

func TestReadLabelsRejectsBadRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(p, []byte(`{"id":"x","kind":"pair","model_label":"same","model_reason":"","human_label":null}`+"\n"), 0o600)
	if _, err := ReadLabels(p); err == nil || !strings.Contains(err.Error(), "bad row") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseLabelsIgnoresUnaskedAndBadLabels(t *testing.T) {
	text := "```\n{\"id\":\"m_1\",\"label\":\"post\",\"reason\":\"new\\nresult\"}\n{\"id\":\"m_9\",\"label\":\"post\",\"reason\":\"x\"}\n{\"id\":\"m_2\",\"label\":\"maybe\",\"reason\":\"x\"}\nnot json\n```"
	got := parseLabels(text, map[string]bool{"m_1": true, "m_2": true}, Labels)
	if len(got) != 1 || got["m_1"].Label != "post" || got["m_1"].Reason != "new result" {
		t.Fatalf("%+v", got)
	}
}

func TestLabelDraftWritesSchema(t *testing.T) {
	f := newRunFixture(t)
	f.or.chat = func(system, user string) string {
		// Answer for every id in the data block.
		var out []string
		data := user[strings.Index(user, "<data>")+7 : strings.Index(user, "</data>")]
		var rows []map[string]any
		json.Unmarshal([]byte(strings.TrimSpace(data)), &rows)
		label := "digest"
		if strings.Contains(system, "same underlying story") {
			label = "different"
		}
		for _, r := range rows {
			out = append(out, fmt.Sprintf(`{"id":%q,"label":%q,"reason":"because"}`, r["id"], label))
		}
		return strings.Join(out, "\n")
	}
	outPath := filepath.Join(t.TempDir(), "labels.jsonl")
	var log bytes.Buffer
	err := LabelDraft(context.Background(), LabelOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), Model: f.or.model(1),
		Out: outPath, Source: "llm-papers", NItems: 2, NPairs: 2, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	rows, err := ReadLabels(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var items, pairs int
	for _, r := range rows {
		if r.HumanLabel != nil || r.ModelReason != "because" {
			t.Fatalf("row: %+v", r)
		}
		switch r.Kind {
		case "item":
			items++
			if r.ModelLabel != "digest" || len(r.Links) != 1 || r.Title == "" {
				t.Fatalf("item row: %+v", r)
			}
		case "pair":
			pairs++
			if r.ModelLabel != "different" || r.Sim == nil || r.A.ID == "" || r.ID != r.A.ID+"+"+r.B.ID {
				t.Fatalf("pair row: %+v", r)
			}
		}
	}
	if items != 2 || pairs != 2 {
		t.Fatalf("items=%d pairs=%d", items, pairs)
	}
	// The raw line has human_label: null (the review page fills it in).
	raw, _ := os.ReadFile(outPath)
	if !strings.Contains(string(raw), `"human_label":null`) {
		t.Fatalf("no human_label null in %s", raw)
	}
	// It never overwrites.
	if err := LabelDraft(context.Background(), LabelOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), Model: f.or.model(1),
		Out: outPath, Source: "llm-papers", NItems: 1, Log: &log}); err == nil {
		t.Fatal("overwrote an existing labels file")
	}
}

func TestAgreeAgainstFakeLookups(t *testing.T) {
	f := newRunFixture(t)
	// Items in the fixture are 1-3h old; move the clock so they are 50h+.
	now := t0.Add(50 * time.Hour)
	hn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		hits := []map[string]any{}
		switch q {
		case "2610.00001": // popular; a pdf link must still match by canonical key
			hits = append(hits, map[string]any{"url": "https://arxiv.org/pdf/2610.00001v2", "points": 250},
				map[string]any{"url": "https://arxiv.org/abs/2610.00001999", "points": 900}) // a different paper
		case "2610.00002":
			hits = append(hits, map[string]any{"url": "https://arxiv.org/abs/2610.00002", "points": 120})
		case "2610.00003":
			hits = append(hits, map[string]any{"url": "https://arxiv.org/abs/2610.00003", "points": 5})
		}
		okJSON(w, map[string]any{"hits": hits})
	}))
	defer hn.Close()
	lob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, []map[string]any{{"url": r.URL.Query().Get("url"), "score": 7}})
	}))
	defer lob.Close()
	rep, err := Agree(context.Background(), AgreeOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), Model: f.or.model(1),
		MinPoints: 100, MinAgeHours: 48, Now: func() time.Time { return now }, HNBase: hn.URL, LobstersBase: lob.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Counts()
	// Paper A (250 pts, post 0.9) posted; Paper B (120 pts, post 0.45) dropped.
	if rep.Checked != 3 || rep.OnHN != 3 || rep.OnLobsters != 3 || len(rep.Popular) != 2 || c["post"] != 1 || c["drop"] != 1 {
		t.Fatalf("report: %+v counts %v", rep, c)
	}
	if rep.Popular[0].HN != 250 || rep.Popular[0].Decision != "post" || rep.Popular[0].Source != "fresh" {
		t.Fatalf("first: %+v", rep.Popular[0])
	}
	// Nothing was submitted.
	if n := f.hub.count("msg") + f.hub.count("channel"); n != 0 {
		t.Fatalf("agree submitted %d object(s)", n)
	}
}

func TestAgreeStopsAskingLobstersAfter429(t *testing.T) {
	f := newRunFixture(t)
	hn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"hits": []any{}})
	}))
	defer hn.Close()
	var lobHits int
	lob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lobHits++
		w.WriteHeader(429)
	}))
	defer lob.Close()
	rep, err := Agree(context.Background(), AgreeOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), MinPoints: 100, MinAgeHours: 48,
		Now: func() time.Time { return t0.Add(50 * time.Hour) }, HNBase: hn.URL, LobstersBase: lob.URL})
	if err != nil {
		t.Fatal(err)
	}
	if lobHits != 1 || rep.LobstersSkipped != 2 || rep.LookupFails != 1 || rep.Checked != 3 {
		t.Fatalf("lobsters hits=%d report=%+v", lobHits, rep)
	}
}

func TestLabelDraftSkipResumes(t *testing.T) {
	f := newRunFixture(t)
	f.or.chat = func(system, user string) string {
		data := user[strings.Index(user, "<data>")+7 : strings.Index(user, "</data>")]
		var rows []map[string]any
		json.Unmarshal([]byte(strings.TrimSpace(data)), &rows)
		var out []string
		for _, r := range rows {
			l := "drop"
			if strings.Contains(system, "same underlying story") {
				l = "same"
			}
			out = append(out, fmt.Sprintf(`{"id":%q,"label":%q,"reason":"r"}`, r["id"], l))
		}
		return strings.Join(out, "\n")
	}
	dir := t.TempDir()
	first, second := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	opts := LabelOptions{Config: f.cfg, Hub: NewHub(f.hub.srv.URL, nil), Model: f.or.model(1), Source: "llm-papers", NItems: 2, NPairs: 1, Log: io.Discard}
	opts.Out = first
	if err := LabelDraft(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	done, _ := ReadLabels(first)
	opts.Out, opts.Skip = second, map[string]bool{}
	for _, r := range done {
		opts.Skip[r.ID] = true
	}
	if err := LabelDraft(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	again, _ := ReadLabels(second)
	if len(done) != 3 || len(again) != 2 { // 1 item left; 1 new pair of the 3 possible
		t.Fatalf("first %d rows, second %d rows", len(done), len(again))
	}
	for _, r := range again {
		if opts.Skip[r.ID] {
			t.Fatalf("row %s drafted twice", r.ID)
		}
	}
}
