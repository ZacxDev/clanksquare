package editor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PreviewDir is where a preview run keeps its own state and its logs, apart
// from the live state file.
func PreviewDir(stateDir string) string { return filepath.Join(stateDir, "preview") }

// PreviewFile is the JSONL file a preview run on that UTC date appends to.
func PreviewFile(stateDir, date string) string {
	return filepath.Join(PreviewDir(stateDir), "preview-"+date+".jsonl")
}

// PreviewRow is one line of a preview file: one decided story of one topic
// in the pick window, as ranked by that run. Status is "picked" (would post
// now), "not_picked" (eligible, outside today's budget or below the floor),
// "posted" (picked or posted on an earlier run) or "pending".
type PreviewRow struct {
	RunAt    int64              `json:"run_at"`
	Date     string             `json:"date"` // UTC
	Topic    string             `json:"topic"`
	Story    string             `json:"story"`
	Rank     int                `json:"rank"` // 1-based, among every decided story in the window
	Status   string             `json:"status"`
	Label    string             `json:"label"` // the probability gate's label (recorded only)
	Probs    map[string]float64 `json:"probs"`
	Model    string             `json:"model"`
	Created  int64              `json:"created"`
	URL      string             `json:"url"`
	Title    string             `json:"title"` // untrusted feed text, as the story post would carry it
	Sources  []string           `json:"sources"`
	Budget   int                `json:"budget"`
	MinPPost float64            `json:"min_p_post"`
}

func (r *runner) previewDate() string { return r.Now().UTC().Format("2006-01-02") }

// previewWrite appends one row per decided story in the window, in rank order.
func (r *runner) previewWrite(t *Topic, cs *ChannelState, ranked []string, picked map[string]bool) {
	path := filepath.Join(r.StateDir, "preview-"+r.previewDate()+".jsonl")
	r.res.PreviewFile = path
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		r.fail("preview file: %v", err)
		return
	}
	defer f.Close()
	for i, sid := range ranked {
		st := r.state.Stories[sid]
		d := st.Decisions[t.Slug]
		status := "not_picked"
		_, seen := cs.Seen[sid]
		_, pending := cs.Pending[sid]
		switch {
		case picked[sid]:
			status = "picked"
		case pending:
			status = "pending"
		case seen:
			status = "posted"
		}
		url := ""
		if len(st.Canon) > 0 {
			url = st.Canon[0]
		}
		row := PreviewRow{RunAt: r.nowMS(), Date: r.previewDate(), Topic: t.Slug, Story: sid, Rank: i + 1, Status: status,
			Label: d.Label, Probs: d.Probs, Model: d.Model, Created: st.Created, URL: url, Title: oneLine(st.Items[0].Title),
			Sources: storySources(st), Budget: r.Config.Decide.TopKPerDay, MinPPost: r.Config.Decide.MinPPost}
		raw, err := json.Marshal(row)
		if err != nil {
			r.fail("preview row %s: %v", sid, err)
			return
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			r.fail("preview file: %v", err)
			return
		}
	}
}

// previewSummary prints the run's one-line summary (for Loki).
func (r *runner) previewSummary() {
	var parts []string
	for _, t := range r.res.Topics {
		parts = append(parts, fmt.Sprintf("%s:decided=%d,eligible=%d,today=%d,picked=%d,budget=%d", t.Slug, t.Decided, t.Eligible, t.Today, t.Posted, t.Budget))
	}
	fmt.Fprintf(r.Out, "PREVIEW-SUMMARY date=%s model=%s question=%s items=%d new_stories=%d topics=%s errors=%d file=%s\n",
		r.previewDate(), r.Config.DecisionModel, r.Config.Decide.Question, r.res.Items, r.res.NewStories,
		strings.Join(parts, ";"), len(r.res.Errors), r.res.PreviewFile)
}
