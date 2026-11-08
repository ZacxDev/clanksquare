package editor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/ZacxDev/clanksquare/internal/canon"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// Popularity truth: a label per item derived from a signal outside the
// editor (Hugging Face Daily Papers upvotes for arXiv papers, HN points for
// everything else), so eval can score the decision model against something
// no model wrote. An item with no signal is unknown (""), never drop.

// Signal names in a TruthRow.
const (
	SignalHF      = "hf_upvotes"
	SignalHN      = "hn_points"
	SignalRelease = "release_default" // a fixed label, not a measurement
)

// TruthRow is one item's popularity label. It holds ids, the canonical URL,
// the signal value and the label; never the item's title or text.
type TruthRow struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	URL     string `json:"url"`
	ItemTS  int64  `json:"item_ts"`
	Signal  string `json:"signal,omitempty"`
	Value   *int   `json:"value"` // nil: no measurement
	// Label is post | digest | drop, or "" when unknown (see Why).
	Label     string `json:"label"`
	Why       string `json:"why,omitempty"`
	CheckedAt int64  `json:"checked_at"`
	// Excluded, when set, keeps the row out of eval's axis 1 whatever its
	// label: "hf_daily_only" is an item whose only source is HF Daily Papers,
	// which HF's own upvotes would grade (circular; see hfDailyOnly).
	Excluded string `json:"excluded,omitempty"`
}

// HFDailyVia is the via label the curator's hfdaily source kind always
// writes (curator.HFDailySource; the curator rejects a name override for that
// kind, so the label is fixed).
const HFDailyVia = "Hugging Face Daily Papers"

// ExcludedHFDailyOnly is TruthRow.Excluded for an item only HF Daily Papers
// carried.
const ExcludedHFDailyOnly = "hf_daily_only"

// hfDailyOnly returns the ids of the items whose only source is HF Daily
// Papers: every item read with the same canonical URL (itself included) came
// via HFDailyVia. An item that some other source also carried still counts:
// that source picked it without looking at HF's votes.
func hfDailyOnly(items []Item) map[string]bool {
	key := func(it Item) string {
		if it.Canon != "" {
			return it.Canon
		}
		return "id:" + it.ID
	}
	other := map[string]bool{}
	for _, it := range items {
		if it.Via != HFDailyVia {
			other[key(it)] = true
		}
	}
	out := map[string]bool{}
	for _, it := range items {
		if it.Via == HFDailyVia && !other[key(it)] {
			out[it.ID] = true
		}
	}
	return out
}

// recheckable are the unknown answers a lookup gave: cached for
// truth.recheck_hours. A failed lookup ("lookup_failed") is not an answer
// and is retried on every run; too_new and no_url make no request.
var recheckable = map[string]bool{
	"not_on_hf": true, "not_on_daily": true, "unsettled": true, "no_upvotes_field": true,
	"bad_daily_date": true, "not_on_hn": true,
}

// TruthLabel maps a signal value to a label: post at or above post, digest
// at or above digest, drop below. Callers only call it with a measured value;
// "no signal" never reaches it.
func TruthLabel(value, post, digest int) string {
	switch {
	case value >= post:
		return "post"
	case value >= digest:
		return "digest"
	}
	return "drop"
}

// relabel recomputes a cached row's label from its measured value.
func relabel(r TruthRow, tc TruthConfig) TruthRow {
	switch {
	case r.Value == nil:
	case r.Signal == SignalHF:
		r.Label = TruthLabel(*r.Value, tc.HFPost, tc.HFDigest)
	case r.Signal == SignalHN:
		r.Label = TruthLabel(*r.Value, tc.HNPost, tc.HNDigest)
	}
	if r.Signal == SignalRelease {
		r.Label = tc.ReleaseLabel
	}
	return r
}

// TruthPath is the cache file in a state directory.
func TruthPath(dir string) string { return filepath.Join(dir, "truth.json") }

// ReadTruth reads a truth file (a JSON array of rows) into a map by id.
func ReadTruth(path string) (map[string]TruthRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []TruthRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("truth %s: %w", path, err)
	}
	out := make(map[string]TruthRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

func writeTruth(path string, rows map[string]TruthRow) error {
	list := make([]TruthRow, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Channel != list[j].Channel {
			return list[i].Channel < list[j].Channel
		}
		return list[i].ID < list[j].ID
	})
	raw, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	return hubkit.WriteFileAtomic(path, append(raw, '\n'))
}

// TruthOptions configure truth.
type TruthOptions struct {
	Config   *Config
	Hub      *hubkit.Client
	HTTP     *http.Client
	StateDir string
	Refresh  bool // look up rows the cache already labels, too
	Now      func() time.Time
	// HFBase and HNBase override the lookup endpoints (tests).
	HFBase string
	HNBase string
	Delay  time.Duration
	Log    io.Writer
}

// TruthReport summarises a truth pass.
type TruthReport struct {
	Rows map[string]TruthRow
	// Per channel: items, items old enough, and labelled items.
	Channels []string
	Items    map[string]int
	Eligible map[string]int
	Labelled map[string]int
	Labels   map[string]map[string]int // channel -> label -> n
	Why      map[string]int            // reason -> n, for unknown rows
	Lookups  int
	Cached   int
	// Deferred: unknown rows not looked up again because their last answer
	// is younger than truth.recheck_hours.
	Deferred int
	// Excluded: rows kept out of axis 1 (hf_daily_only).
	Excluded int
	Cfg      TruthConfig
}

// hfPaper is the part of GET /api/papers/<arxiv id> that truth reads.
type hfPaper struct {
	ID                 string `json:"id"`
	Upvotes            *int   `json:"upvotes"`
	SubmittedOnDailyAt string `json:"submittedOnDailyAt"`
}

var errNotFound = errors.New("not found")

func (o *TruthOptions) hfLookup(ctx context.Context, arxivID string) (*hfPaper, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.HFBase+"/api/papers/"+arxivID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET hf paper %s: HTTP %d", arxivID, resp.StatusCode)
	}
	var p hfPaper
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// releaseRE matches a GitHub release page.
var releaseRE = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/releases(/|$)`)

// truthFor looks one item up. It returns the row and whether a lookup
// request was made.
func (o *TruthOptions) truthFor(ctx context.Context, it Item, now int64) TruthRow {
	tc := o.Config.Truth
	row := TruthRow{ID: it.ID, Channel: it.Channel, URL: it.Canon, ItemTS: it.TS, CheckedAt: now}
	minAge := int64(tc.MinAgeHours) * 3600_000
	if now-it.TS < minAge {
		row.Why = "too_new"
		return row
	}
	if it.Canon == "" {
		row.Why = "no_url"
		return row
	}
	if id := canon.ArxivID(it.Canon); id != "" {
		p, err := o.hfLookup(ctx, id)
		switch {
		case errors.Is(err, errNotFound):
			row.Why = "not_on_hf"
		case err != nil:
			row.Why = "lookup_failed"
			fmt.Fprintf(o.Log, "WARN hf lookup %s: %v\n", id, err)
		case p.Upvotes == nil:
			row.Why = "no_upvotes_field"
		case p.SubmittedOnDailyAt == "":
			// A paper page that never went to Daily Papers was never put
			// in front of the voters: its 0 upvotes are not a judgement.
			row.Why = "not_on_daily"
		default:
			at, err := time.Parse(time.RFC3339, p.SubmittedOnDailyAt)
			if err != nil {
				row.Why = "bad_daily_date"
			} else if now-at.UnixMilli() < minAge {
				row.Why = "unsettled"
			} else {
				v := *p.Upvotes
				row.Signal, row.Value, row.Label = SignalHF, &v, TruthLabel(v, tc.HFPost, tc.HFDigest)
			}
		}
		return row
	}
	if releaseRE.MatchString(it.Canon) {
		row.Signal, row.Label = SignalRelease, tc.ReleaseLabel
		return row
	}
	ao := AgreeOptions{HTTP: o.HTTP, HNBase: o.HNBase}
	n, found, err := ao.hnLookup(ctx, it.Canon)
	switch {
	case err != nil:
		row.Why = "lookup_failed"
		fmt.Fprintf(o.Log, "WARN hn lookup %s: %v\n", it.Canon, err)
	case !found:
		row.Why = "not_on_hn"
	default:
		row.Signal, row.Value, row.Label = SignalHN, &n, TruthLabel(n, tc.HNPost, tc.HNDigest)
	}
	return row
}

// Truth derives (or refreshes) a popularity label for every source item and
// caches it in the state directory. Rows the cache already labels are kept
// unless Refresh; unknown rows are looked up again. It submits nothing.
func Truth(ctx context.Context, o TruthOptions) (*TruthReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.HFBase == "" {
		o.HFBase = "https://huggingface.co"
	}
	if o.HNBase == "" {
		o.HNBase = "https://hn.algolia.com"
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.StateDir == "" {
		return nil, errors.New("truth: -state is required (the cache lives there)")
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	rows := map[string]TruthRow{}
	if old, err := ReadTruth(TruthPath(o.StateDir)); err == nil {
		rows = old
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	items, err := ReadSources(ctx, o.Hub, o.Config, o.Log)
	if err != nil {
		return nil, err
	}
	now := o.Now().UnixMilli()
	rep := &TruthReport{Items: map[string]int{}, Eligible: map[string]int{}, Labelled: map[string]int{},
		Labels: map[string]map[string]int{}, Why: map[string]int{}, Cfg: o.Config.Truth}
	recheck := int64(o.Config.Truth.RecheckHours) * 3600_000
	for _, it := range items {
		if old, ok := rows[it.ID]; ok && old.Label != "" && !o.Refresh {
			// The cache keeps the measurement; the label always follows the
			// current thresholds.
			rows[it.ID] = relabel(old, o.Config.Truth)
			rep.Cached++
			continue
		}
		if old, ok := rows[it.ID]; ok && old.Label == "" && recheckable[old.Why] && now-old.CheckedAt < recheck && !o.Refresh {
			rep.Deferred++
			continue
		}
		row := o.truthFor(ctx, it, now)
		if row.Why != "too_new" && row.Why != "no_url" && row.Signal != SignalRelease {
			rep.Lookups++
			if o.Delay > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(o.Delay):
				}
			}
		}
		rows[it.ID] = row
	}
	// Recomputed on every pass from the items read now, so an item that a
	// second source later carries is scored again.
	only := hfDailyOnly(items)
	for _, it := range items {
		r := rows[it.ID]
		r.Excluded = ""
		if only[it.ID] {
			r.Excluded = ExcludedHFDailyOnly
			rep.Excluded++
		}
		rows[it.ID] = r
	}
	if err := writeTruth(TruthPath(o.StateDir), rows); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, it := range items {
		r := rows[it.ID]
		if !seen[r.Channel] {
			seen[r.Channel] = true
			rep.Channels = append(rep.Channels, r.Channel)
		}
		rep.Items[r.Channel]++
		if r.Why != "too_new" {
			rep.Eligible[r.Channel]++
		}
		if r.Label != "" {
			rep.Labelled[r.Channel]++
			if rep.Labels[r.Channel] == nil {
				rep.Labels[r.Channel] = map[string]int{}
			}
			rep.Labels[r.Channel][r.Label]++
		} else {
			rep.Why[r.Why]++
		}
	}
	rep.Rows = rows
	return rep, nil
}

// Quantile is the q-quantile (nearest rank) of sorted values.
func Quantile(sorted []int, q float64) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}

// Write prints the report: coverage per channel, the signal distributions
// and the label counts.
func (r *TruthReport) Write(w io.Writer) {
	c := r.Cfg
	fmt.Fprintf(w, "truth min_age_hours=%d recheck_hours=%d hf post>=%d digest>=%d hn post>=%d digest>=%d release=%s lookups=%d cached=%d deferred=%d excluded_hf_daily_only=%d\n",
		c.MinAgeHours, c.RecheckHours, c.HFPost, c.HFDigest, c.HNPost, c.HNDigest, c.ReleaseLabel, r.Lookups, r.Cached, r.Deferred, r.Excluded)
	for _, ch := range r.Channels {
		l := r.Labels[ch]
		fmt.Fprintf(w, "channel=%s items=%d old_enough=%d labelled=%d coverage=%.3f (of old enough) post=%d digest=%d drop=%d\n",
			ch, r.Items[ch], r.Eligible[ch], r.Labelled[ch], ratio(r.Labelled[ch], r.Eligible[ch]), l["post"], l["digest"], l["drop"])
	}
	whys := make([]string, 0, len(r.Why))
	for k := range r.Why {
		whys = append(whys, k)
	}
	sort.Strings(whys)
	fmt.Fprintf(w, "unknown:")
	for _, k := range whys {
		fmt.Fprintf(w, " %s=%d", k, r.Why[k])
	}
	fmt.Fprintln(w)
	for _, sig := range []string{SignalHF, SignalHN} {
		var vs []int
		for _, row := range r.Rows {
			if row.Signal == sig && row.Value != nil {
				vs = append(vs, *row.Value)
			}
		}
		if len(vs) == 0 {
			fmt.Fprintf(w, "distribution %s n=0\n", sig)
			continue
		}
		sort.Ints(vs)
		fmt.Fprintf(w, "distribution %s n=%d min=%d p25=%d p50=%d p75=%d p85=%d p90=%d p95=%d max=%d\n", sig, len(vs),
			vs[0], Quantile(vs, .25), Quantile(vs, .5), Quantile(vs, .75), Quantile(vs, .85), Quantile(vs, .9), Quantile(vs, .95), vs[len(vs)-1])
	}
}
