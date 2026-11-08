package editor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// RunOptions configure one editor run.
type RunOptions struct {
	Config *Config
	Key    ed25519.PrivateKey
	// StateDir holds state.json and its lock. Required with Post.
	StateDir string
	// Post submits to the hub. Without it the run is a dry run: it reads,
	// clusters and decides (model calls are made and metered), prints what
	// it would post, and submits nothing and writes no state.
	Post bool
	// Preview is an unattended dry run that keeps its own state: it submits
	// nothing and creates no channel, like a dry run, but it keeps stories,
	// decisions and its simulated picks in <StateDir>/preview/state.json (so
	// the daily budget and "already picked" carry from day to day, and no
	// story is decided twice), and appends every decision and pick, with
	// scores, to <StateDir>/preview/preview-YYYY-MM-DD.jsonl (UTC date). It
	// never touches <StateDir>/state.json, so going live later starts from
	// the hub's own log. Requires StateDir; refused with Post.
	Preview bool
	Hub     *hubkit.Client
	Model   *Model
	Now     func() time.Time
	Out     io.Writer

	// afterPost, when set (tests only), runs after each confirmed post and
	// its state write. A panic there simulates a crash mid-run.
	afterPost func(storyID string)
}

// TopicStats is one topic's summary for a run.
type TopicStats struct {
	Slug    string
	Decided int
	Post    int
	Digest  int
	Drop    int
	// Eligible: stories in the pick window, decided, not yet posted.
	Eligible int
	// Today: posted (or, in a preview, picked) earlier in this UTC day,
	// counted from the state and the channel's log; Budget = top_k_per_day.
	Today  int
	Budget int
	Posted int // submitted and confirmed (or, in a dry run, would post)
}

// Result is a run's outcome.
type Result struct {
	Items      int
	NewStories int
	Topics     []TopicStats
	Errors     []string
	// PreviewFile is the JSONL file a preview run appended to.
	PreviewFile string
}

// ExitCode is 0 for a clean run, 1 if anything failed.
func (r *Result) ExitCode() int {
	if len(r.Errors) > 0 {
		return 1
	}
	return 0
}

type runner struct {
	RunOptions
	me      string
	state   *State
	res     *Result
	stopped bool
	blocked map[string]bool
	tried   map[string]bool
}

func (r *runner) logf(level, format string, args ...any) {
	fmt.Fprintf(r.Out, level+" "+format+"\n", args...)
}

func (r *runner) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.res.Errors = append(r.res.Errors, msg)
	r.logf("ERROR", "%s", msg)
}

func (r *runner) nowMS() int64 { return r.Now().UnixMilli() }

func (r *runner) dry() bool { return !r.Post }

// Run performs one editor run.
func Run(ctx context.Context, o RunOptions) *Result {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	r := &runner{RunOptions: o, res: &Result{}, blocked: map[string]bool{}, tried: map[string]bool{}}
	r.me = hubkit.KeyIDOf(o.Key)
	r.logf("INFO", "editor %s key=%s hub=%s post=%v preview=%v", Version, r.me, o.Config.HubURL, o.Post, o.Preview)
	if o.Preview {
		if o.Post {
			r.fail("-preview and -post are exclusive")
			return r.res
		}
		if o.StateDir == "" {
			r.fail("-preview needs -state: it keeps its decisions and picks there")
			return r.res
		}
		r.StateDir = PreviewDir(o.StateDir)
		r.logf("PREVIEW", "nothing is submitted; decisions and picks are kept in %s", r.StateDir)
	} else if r.dry() {
		r.logf("DRY-RUN", "no -post: nothing is submitted and no state is written")
	}
	if o.Post || o.Preview {
		if o.StateDir == "" {
			r.fail("-post needs -state: pending posts must be written ahead of their submit")
			return r.res
		}
		if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
			r.fail("state dir: %v", err)
			return r.res
		}
		unlock, err := hubkit.Lock(StatePath(r.StateDir))
		if err != nil {
			if errors.Is(err, hubkit.ErrLocked) {
				r.fail("another editor run holds %s.lock: posting nothing", StatePath(r.StateDir))
			} else {
				r.fail("cannot lock %s.lock (%v): posting nothing", StatePath(r.StateDir), err)
			}
			return r.res
		}
		defer unlock()
	}
	r.state = newState()
	if r.StateDir != "" {
		s, err := LoadState(r.StateDir)
		if err != nil {
			r.fail("%v", err)
			return r.res
		}
		r.state = s
	}

	items, err := ReadSources(ctx, o.Hub, o.Config, o.Out)
	if err != nil {
		r.fail("%v", err)
		return r.res
	}
	cutoff := r.nowMS() - int64(o.Config.MaxAgeHours)*3600_000
	var recent []Item
	for _, it := range items {
		if it.TS >= cutoff {
			recent = append(recent, it)
		}
	}
	r.res.Items = len(recent)
	cl := &Clusterer{Cfg: o.Config.Cluster, Model: o.Model, EmbedModel: o.Config.EmbedModel,
		DecisionModel: o.Config.DecisionModel, State: r.state, Log: o.Out}
	created, err := cl.Assign(ctx, recent)
	r.res.NewStories = len(created)
	if err != nil {
		r.fail("clustering: %v", err)
		r.saveState() // keep what was assigned before the error
		return r.res
	}
	cl.Close(r.nowMS())
	r.logf("INFO", "items=%d (last %dh) new_stories=%d stories=%d same_story_asked=%d", len(recent), o.Config.MaxAgeHours, len(created), len(r.state.Stories), cl.SameStoryAsked)

	r.resolveAll()
	for i := range o.Config.Topics {
		if r.stopped {
			break
		}
		r.runTopic(ctx, &o.Config.Topics[i], cutoff)
	}
	r.resolveAll()
	r.saveState()
	for id, cs := range r.state.Channels {
		if len(cs.Pending) > 0 {
			r.fail("channel=%s (%s): %d post(s) the hub has neither confirmed nor refused; resent next run", cs.Slug, id, len(cs.Pending))
		}
	}
	for _, t := range r.res.Topics {
		fmt.Fprintf(r.Out, "topic=%s decided=%d post=%d digest=%d drop=%d eligible=%d today=%d budget=%d posted=%d\n",
			t.Slug, t.Decided, t.Post, t.Digest, t.Drop, t.Eligible, t.Today, t.Budget, t.Posted)
	}
	if r.Preview {
		r.previewSummary()
	}
	return r.res
}

func (r *runner) saveState() bool {
	if (r.dry() && !r.Preview) || r.StateDir == "" {
		return true
	}
	if err := r.state.Save(r.StateDir); err != nil {
		r.fail("state write failed (%v): stopping submissions for this run", err)
		r.stopped = true
		return false
	}
	return true
}

func inTopic(t *Topic, st *Story) bool {
	for _, it := range st.Items {
		if contains(t.From, it.Channel) {
			return true
		}
	}
	return false
}

func (r *runner) runTopic(ctx context.Context, t *Topic, cutoff int64) {
	ts := TopicStats{Slug: t.Slug}
	defer func() { r.res.Topics = append(r.res.Topics, ts) }()

	id, ok := r.ensureChannel(ctx, t)
	if !ok {
		return
	}
	var cs *ChannelState
	if id != "" {
		cs = r.state.Channels[id]
		if cs == nil {
			cs = &ChannelState{Slug: t.Slug, Seen: map[string]int64{}}
			r.state.Channels[id] = cs
		}
		// The log is read on every run, not only when the state is lost: what
		// the channel already carries (and when) is counted from the state
		// AND the log, so neither alone can under-count today's posts.
		fromLog, err := r.rebuildSeen(ctx, id)
		if err != nil {
			r.fail("channel=%s could not read its log: %v", t.Slug, err)
			return
		}
		mergeSeen(cs.Seen, fromLog)
	} else if r.Preview {
		// Preview of a channel not created yet: its simulated picks are kept
		// under a key no hub channel id can take.
		key := "preview:" + t.Slug
		cs = r.state.Channels[key]
		if cs == nil {
			cs = &ChannelState{Slug: t.Slug, Seen: map[string]int64{}}
			r.state.Channels[key] = cs
		}
	} else {
		cs = &ChannelState{Slug: t.Slug, Seen: map[string]int64{}} // dry run, channel not yet created
	}

	ids := make([]string, 0, len(r.state.Stories))
	for sid, st := range r.state.Stories {
		if st.Created >= cutoff && inTopic(t, st) {
			ids = append(ids, sid)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := r.state.Stories[ids[i]], r.state.Stories[ids[j]]
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.ID < b.ID
	})
	for _, sid := range ids {
		st := r.state.Stories[sid]
		if st.Decisions[t.Slug] != nil {
			continue
		}
		d, err := DecideStory(ctx, r.Model, r.Config.DecisionModel, r.Config.Decide, t, st, r.nowMS())
		if err != nil {
			r.fail("topic=%s story %s: decision failed: %v", t.Slug, sid, err)
			if errors.Is(err, ErrSpendCap) {
				r.stopped = true
				return
			}
			continue
		}
		if st.Decisions == nil {
			st.Decisions = map[string]*StoryDecision{}
		}
		st.Decisions[t.Slug] = d
		ts.Decided++
		r.logf("INFO", "topic=%s story=%s label=%s raw=%s post=%.3f digest=%.3f drop=%.3f model=%s %q",
			t.Slug, sid, d.Label, d.Raw, d.Probs["post"], d.Probs["digest"], d.Probs["drop"], d.Model, st.Items[0].Title)
	}
	for _, sid := range ids {
		switch r.state.Stories[sid].Decisions[t.Slug].labelOr("") {
		case "post":
			ts.Post++
		case "digest":
			ts.Digest++
		case "drop":
			ts.Drop++
		}
	}
	r.saveState()

	if id != "" && r.blocked[id] {
		r.fail("channel=%s has a post the hub has not confirmed: posting nothing new to it this run", t.Slug)
		return
	}
	cands, ranked := r.candidates(t, cs)
	ts.Eligible = len(cands)
	ts.Today = PostedToday(cs, r.Now())
	ts.Budget = r.Config.Decide.TopKPerDay
	picks := RankPick(cands, min(ts.Budget-ts.Today, t.MaxPerRun), r.Config.Decide.MinPPost)
	r.logf("INFO", "topic=%s eligible=%d posted_today=%d budget=%d min_p_post=%.3f picks=%d", t.Slug, len(cands), ts.Today, ts.Budget, r.Config.Decide.MinPPost, len(picks))
	if r.Preview {
		picked := map[string]bool{}
		for _, p := range picks {
			picked[p.Story] = true
		}
		r.previewWrite(t, cs, ranked, picked)
	}
	for _, p := range picks {
		sid := p.Story
		st := r.state.Stories[sid]
		d := st.Decisions[t.Slug]
		subject, body := FormatStory(st, d)
		if r.dry() {
			verb := "DRY-RUN"
			if r.Preview {
				verb = "PREVIEW"
				cs.Seen[sid] = r.nowMS() // simulated: tomorrow's budget and eligibility see it
			}
			r.logf(verb, "channel=%s would post %q p_post=%.3f", t.Slug, subject, p.PPost)
			for _, l := range strings.Split(body, "\n") {
				fmt.Fprintf(r.Out, "  | %s\n", l)
			}
			ts.Posted++
			continue
		}
		if r.stopped || r.blocked[id] {
			return
		}
		token := clankmail.Sign(r.Key, hubkit.MsgPayload(r.Key, id, subject, body, r.nowMS()))
		if cs.Pending == nil {
			cs.Pending = map[string]string{}
		}
		cs.Pending[sid] = token // write-ahead: on disk before the hub sees it
		if !r.saveState() {
			delete(cs.Pending, sid)
			return
		}
		what := "channel=" + t.Slug + " post " + sid
		out, err := r.send(ctx, what, token)
		switch out {
		case confirmed:
			delete(cs.Pending, sid)
			cs.Seen[sid] = r.nowMS()
			ts.Posted++
			if !r.saveState() {
				return
			}
			if r.afterPost != nil {
				r.afterPost(sid)
			}
		case refused:
			delete(cs.Pending, sid)
			r.fail("%s: submit refused: %v", what, err)
			r.saveState()
		default:
			r.blocked[id] = true
			r.logf("WARN", "%s: unconfirmed (%v): will resend the same token", what, err)
			return
		}
	}
}

// candidates returns the topic's eligible stories (see pick.go) and every
// decided story in the pick window, ranked (P(post) descending, id), for the
// preview log.
func (r *runner) candidates(t *Topic, cs *ChannelState) (eligible []Candidate, ranked []string) {
	since := r.nowMS() - int64(r.Config.Decide.WindowHours)*3600_000
	var all []Candidate
	for sid, st := range r.state.Stories {
		d := st.Decisions[t.Slug]
		if d == nil || st.Created < since || !inTopic(t, st) {
			continue
		}
		all = append(all, Candidate{Story: sid, PPost: d.Probs["post"]})
		if _, ok := cs.Seen[sid]; ok {
			continue
		}
		if _, ok := cs.Pending[sid]; ok {
			continue
		}
		eligible = append(eligible, Candidate{Story: sid, PPost: d.Probs["post"]})
	}
	for _, c := range RankPick(all, len(all), 0) {
		ranked = append(ranked, c.Story)
	}
	return eligible, ranked
}

func (d *StoryDecision) labelOr(def string) string {
	if d == nil {
		return def
	}
	return d.Label
}

// Channel policy for top channels: anyone may read and join, only the
// editor posts.
const (
	policyRead = "public"
	policyPost = "owner"
	policyJoin = "open"
)

func topicManifest(priv ed25519.PrivateKey, t *Topic, channelID string, ts int64, mods ...string) []byte {
	return hubkit.Encode(struct {
		hubkit.Common
		Channel string   `json:"channel,omitempty"`
		Slug    string   `json:"slug"`
		Title   string   `json:"title"`
		About   string   `json:"about,omitempty"`
		Tags    []string `json:"tags,omitempty"`
		Read    string   `json:"read"`
		Post    string   `json:"post"`
		Join    string   `json:"join"`
		Mods    []string `json:"mods,omitempty"`
	}{hubkit.Header(priv, "channel", ts), channelID, t.Slug, t.Title, t.About, t.Tags, policyRead, policyPost, policyJoin, mods})
}

func manifestDiffers(t *Topic, f map[string]any) bool {
	str := func(k string) string { s, _ := f[k].(string); return s }
	if str("slug") != t.Slug || str("title") != t.Title || str("about") != t.About ||
		str("read") != policyRead || str("post") != policyPost || str("join") != policyJoin {
		return true
	}
	have := hubkit.StrList(f["tags"])
	if len(have) != len(t.Tags) {
		return true
	}
	for i := range have {
		if have[i] != t.Tags[i] {
			return true
		}
	}
	return false
}

// ErrNotOwner is the refusal for a top channel another key owns.
var ErrNotOwner = errors.New("owned by another key")

// ensureChannel returns the topic channel's id, creating it (post: owner)
// if it is missing and updating its manifest if it differs. A channel owned
// by another key is refused: the editor never posts into it. "" with
// ok=true is a dry run of a channel that does not exist yet.
func (r *runner) ensureChannel(ctx context.Context, t *Topic) (string, bool) {
	resp, err := r.Hub.ChannelBySlug(ctx, t.Slug)
	if hubkit.IsCode(err, "E_NOT_FOUND") {
		payload := topicManifest(r.Key, t, "", r.nowMS())
		if r.dry() {
			r.logf("DRY-RUN", "channel=%s would create channel (read: %s, post: %s, join: %s)", t.Slug, policyRead, policyPost, policyJoin)
			return "", true
		}
		if _, ok := r.submit(ctx, "channel #"+t.Slug+" genesis", payload); !ok {
			r.fail("channel=%s could not be created", t.Slug)
			return "", false
		}
		id := clankmail.ChannelID(payload)
		r.logf("INFO", "channel=%s created %s", t.Slug, id)
		return id, true
	}
	if err != nil {
		r.fail("channel=%s lookup failed: %v", t.Slug, err)
		return "", false
	}
	gen, man, verr := hubkit.VerifyChannel(resp, t.Slug)
	if verr != nil {
		r.fail("channel=%s hub served a bad channel record: %v", t.Slug, verr)
		return "", false
	}
	if gen.From != r.me {
		r.fail("channel=%s refused: #%s is %v (%s), not this editor key %s", t.Slug, t.Slug, ErrNotOwner, gen.From, r.me)
		return "", false
	}
	if manifestDiffers(t, man.Fields) {
		ts := max(r.nowMS(), man.Fields["ts"].(int64)+1)
		payload := topicManifest(r.Key, t, resp.Channel, ts, hubkit.StrList(man.Fields["mods"])...)
		if r.dry() {
			r.logf("DRY-RUN", "channel=%s manifest differs from config: would update", t.Slug)
			return resp.Channel, true
		}
		if _, ok := r.submit(ctx, "channel #"+t.Slug+" update", payload); !ok {
			r.fail("channel=%s could not be updated", t.Slug)
			return "", false
		}
		r.logf("INFO", "channel=%s manifest updated", t.Slug)
	}
	return resp.Channel, true
}

// rebuildSeen derives a top channel's posted stories from its log: every
// verified msg to it signed by this key whose body parses as a story.
func (r *runner) rebuildSeen(ctx context.Context, channelID string) (map[string]int64, error) {
	seen := map[string]int64{}
	err := r.Hub.EachLogPage(ctx, channelID, func(toks []string) {
		for _, tok := range toks {
			obj, err := clankmail.Verify(tok)
			if err != nil || obj.Type != "msg" || obj.From != r.me {
				continue
			}
			to := hubkit.StrList(obj.Fields["to"])
			if len(to) != 1 || to[0] != channelID {
				continue
			}
			body, _ := obj.Fields["body"].(string)
			h, err := ParseStory(body)
			if err != nil {
				continue
			}
			at := obj.Fields["ts"].(int64)
			if prev, ok := seen[h.Story]; !ok || at < prev {
				seen[h.Story] = at
			}
		}
	})
	if err != nil {
		return nil, err
	}
	r.logf("INFO", "read %s's log: %d posted stories", channelID, len(seen))
	return seen, nil
}

// ---- submission (the curator's classification, see curator/run.go) ----

type outcome int

const (
	confirmed outcome = iota
	refused
	unknown
)

// notStoredCodes settle a token as not stored (see curator/run.go).
var notStoredCodes = map[string]int{"E_STALE": http.StatusBadRequest, "E_NOT_FOUND": http.StatusNotFound}

var errWrongID = errors.New("wrong id")

func (r *runner) send(ctx context.Context, what, token string) (outcome, error) {
	want := hubkit.TokenID(token)
	id, err := r.Hub.Submit(ctx, token)
	if err == nil {
		if id != want {
			return unknown, fmt.Errorf("%w: hub returned id %s, want %s", errWrongID, id, want)
		}
		return confirmed, nil
	}
	if he, ok := err.(*hubkit.Error); ok {
		switch {
		case he.Status == http.StatusTooManyRequests:
			if !r.stopped {
				r.fail("%s: rate limited (HTTP 429 %s): stopping submissions for this run", what, he.Code)
			}
			r.stopped = true
			return unknown, err
		case he.Code != "" && notStoredCodes[he.Code] == he.Status:
			return refused, err
		}
	}
	return unknown, err
}

func (r *runner) submit(ctx context.Context, what string, payload []byte) (string, bool) {
	token := clankmail.Sign(r.Key, payload)
	out, err := r.send(ctx, what, token)
	switch {
	case out == confirmed:
		return hubkit.TokenID(token), true
	case out == unknown && !r.stopped && !errors.Is(err, errWrongID) && r.Hub.Stored(ctx, hubkit.TokenID(token), token):
		r.logf("INFO", "%s: submit errored (%v) but the hub holds it", what, err)
		return hubkit.TokenID(token), true
	case !r.stopped:
		r.fail("%s: submit failed: %v", what, err)
	}
	return "", false
}

const resolveTimeout = 15 * time.Second

// resolveAll resends every pending token not yet tried this run.
func (r *runner) resolveAll() {
	if r.dry() {
		for id, cs := range r.state.Channels {
			if len(cs.Pending) > 0 {
				r.logf("DRY-RUN", "channel=%s (%s): would resend %d pending post(s)", cs.Slug, id, len(cs.Pending))
			}
		}
		return
	}
	ids := make([]string, 0, len(r.state.Channels))
	for id := range r.state.Channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cs := r.state.Channels[id]
		keys := make([]string, 0, len(cs.Pending))
		for k := range cs.Pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if r.tried[id+" "+k] || r.stopped {
				r.blocked[id] = true
				continue
			}
			r.tried[id+" "+k] = true
			ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
			out, err := r.send(ctx, "channel="+cs.Slug+" resend "+k, cs.Pending[k])
			cancel()
			switch out {
			case confirmed:
				delete(cs.Pending, k)
				cs.Seen[k] = r.nowMS()
				r.logf("INFO", "channel=%s pending post %s confirmed by the hub", cs.Slug, k)
			case refused:
				delete(cs.Pending, k)
				r.logf("WARN", "channel=%s pending post %s was not stored (%v): dropped; the story may post again", cs.Slug, k, err)
			default:
				r.blocked[id] = true
				r.logf("WARN", "channel=%s pending post %s still unconfirmed (%v)", cs.Slug, k, err)
			}
			r.saveState()
		}
	}
}
