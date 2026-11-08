package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/clanksquare/internal/canon"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

func canonOf(link string) string { return canon.Key(link) }

// AgreeOptions configure agree.
type AgreeOptions struct {
	Config *Config
	Hub    *hubkit.Client
	Model  *Model // decides items with no decision in the state; nil: skip them
	// StateDir, if set, supplies decisions already made by run.
	StateDir    string
	HTTP        *http.Client
	MinPoints   int
	MinAgeHours int
	MaxAgeHours int
	Now         func() time.Time
	// HNBase and LobstersBase override the lookup endpoints (tests).
	HNBase       string
	LobstersBase string
	// Delay between lookups, to be polite to both sites.
	Delay time.Duration
	Log   io.Writer
}

// AgreeRow is one popular item and what the editor did with it.
type AgreeRow struct {
	ItemID   string
	Title    string
	URL      string
	HN       int
	Lobsters int
	Decision string // post | digest | drop, or "" when undecided
	Source   string // "state" or "fresh"
}

// AgreeReport is the result of agree.
type AgreeReport struct {
	Checked     int // items old enough that were looked up
	LookupFails int
	OnHN        int // with any HN submission
	OnLobsters  int
	MinPoints   int
	Popular     []AgreeRow // HN points >= MinPoints
}

// Counts returns how many popular items got each decision.
func (r *AgreeReport) Counts() map[string]int {
	c := map[string]int{}
	for _, p := range r.Popular {
		c[p.Decision]++
	}
	return c
}

// Write prints the report.
func (r *AgreeReport) Write(w io.Writer) {
	c := r.Counts()
	fmt.Fprintf(w, "agree checked=%d lookup_failures=%d on_hn=%d on_lobsters=%d hn_points>=%d: %d (post=%d digest=%d drop=%d undecided=%d)\n",
		r.Checked, r.LookupFails, r.OnHN, r.OnLobsters, r.MinPoints, len(r.Popular), c["post"], c["digest"], c["drop"], c[""])
	for _, p := range r.Popular {
		fmt.Fprintf(w, "  hn=%d lobsters=%d decision=%s (%s) %q %s\n", p.HN, p.Lobsters, p.Decision, p.Source, p.Title, p.URL)
	}
}

func (o *AgreeOptions) getJSON(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// hnQuery is the Algolia query for a canonical URL: the arXiv id for a paper
// (so abs, pdf and html submissions all match), else the URL without scheme.
func hnQuery(key string) string {
	if id := canon.ArxivID(key); id != "" {
		return id
	}
	return strings.TrimPrefix(key, "https://")
}

// hnPoints is the highest HN score of a submission whose URL has the same
// canonical key.
func (o *AgreeOptions) hnPoints(ctx context.Context, key string) (int, error) {
	q := url.Values{"restrictSearchableAttributes": {"url"}, "query": {hnQuery(key)}, "hitsPerPage": {"50"}}
	var out struct {
		Hits []struct {
			URL    string `json:"url"`
			Points int    `json:"points"`
		} `json:"hits"`
	}
	if err := o.getJSON(ctx, o.HNBase+"/api/v1/search?"+q.Encode(), &out); err != nil {
		return 0, err
	}
	best := 0
	for _, h := range out.Hits {
		if canon.Key(h.URL) == key && h.Points > best {
			best = h.Points
		}
	}
	return best, nil
}

// lobstersScore is the highest Lobsters score of a story for this link.
func (o *AgreeOptions) lobstersScore(ctx context.Context, link, key string) (int, error) {
	var out []struct {
		URL   string `json:"url"`
		Score int    `json:"score"`
	}
	if err := o.getJSON(ctx, o.LobstersBase+"/stories/url/all.json?url="+url.QueryEscape(link), &out); err != nil {
		return 0, err
	}
	best := 0
	for _, s := range out {
		if canon.Key(s.URL) == key && s.Score > best {
			best = s.Score
		}
	}
	return best, nil
}

// Agree looks up every source item between MinAgeHours and MaxAgeHours old
// on HN (Algolia) and Lobsters, and reports what the editor decided for the
// ones with at least MinPoints HN points. It is read-only: it submits
// nothing and writes no state. Decisions come from the state when it has
// one for the item's story, else (with a Model) from a fresh decision of the
// item as a one-item story.
func Agree(ctx context.Context, o AgreeOptions) (*AgreeReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.HNBase == "" {
		o.HNBase = "https://hn.algolia.com"
	}
	if o.LobstersBase == "" {
		o.LobstersBase = "https://lobste.rs"
	}
	if o.MaxAgeHours == 0 {
		o.MaxAgeHours = 7 * 24
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	st := newState()
	if o.StateDir != "" {
		s, err := LoadState(o.StateDir)
		if err != nil {
			return nil, err
		}
		st = s
	}
	items, err := ReadSources(ctx, o.Hub, o.Config, o.Log)
	if err != nil {
		return nil, err
	}
	now := o.Now().UnixMilli()
	rep := &AgreeReport{MinPoints: o.MinPoints}
	done := map[string]bool{}
	for _, it := range items {
		age := now - it.TS
		if it.Canon == "" || age < int64(o.MinAgeHours)*3600_000 || age > int64(o.MaxAgeHours)*3600_000 || done[it.Canon] {
			continue
		}
		done[it.Canon] = true
		rep.Checked++
		hn, err := o.hnPoints(ctx, it.Canon)
		if err != nil {
			rep.LookupFails++
			fmt.Fprintf(o.Log, "WARN hn lookup %s: %v\n", it.Canon, err)
		}
		lob, err := o.lobstersScore(ctx, it.Target, it.Canon)
		if err != nil {
			rep.LookupFails++
			fmt.Fprintf(o.Log, "WARN lobsters lookup %s: %v\n", it.Canon, err)
		}
		if hn > 0 {
			rep.OnHN++
		}
		if lob > 0 {
			rep.OnLobsters++
		}
		if hn >= o.MinPoints {
			row := AgreeRow{ItemID: it.ID, Title: it.Title, URL: it.Canon, HN: hn, Lobsters: lob}
			topic := topicFor(o.Config, it.Channel)
			if sid, ok := st.Items[it.ID]; ok && topic != nil && st.Stories[sid] != nil && st.Stories[sid].Decisions[topic.Slug] != nil {
				row.Decision, row.Source = st.Stories[sid].Decisions[topic.Slug].Label, "state"
			} else if o.Model != nil && topic != nil {
				one := &Story{ID: storyID(it.ID), Items: []StoryItem{toStoryItem(it)}}
				d, err := DecideStory(ctx, o.Model, o.Config.DecisionModel, o.Config.Decide, topic, one, now)
				if err != nil {
					return nil, fmt.Errorf("deciding %s: %w", it.ID, err)
				}
				row.Decision, row.Source = d.Label, "fresh"
			} else if topic == nil {
				row.Source = "no topic reads " + it.Channel
			}
			rep.Popular = append(rep.Popular, row)
		}
		if o.Delay > 0 {
			select {
			case <-ctx.Done():
				return rep, ctx.Err()
			case <-time.After(o.Delay):
			}
		}
	}
	sort.SliceStable(rep.Popular, func(i, j int) bool { return rep.Popular[i].HN > rep.Popular[j].HN })
	return rep, nil
}
