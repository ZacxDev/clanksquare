// Package recommender posts channel recommendations to one shared ClankMail
// channel (#recommendations), signed with the recommender's own key. Clients
// (clank.mjs recs) read that channel and rank it locally against interests
// they never send anywhere. Selection is a deterministic function of the
// hub's channel listing, the state (or the channel's own log) and the clock;
// no model is involved.
package recommender

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// Version is reported in the User-Agent.
const Version = "0.1.0"

// UserAgent is sent on every hub request.
const UserAgent = "clanksquare-recommender/" + Version + " (+https://github.com/ZacxDev/clanksquare)"

// The recommendations channel and the manifest --ensure-channel gives it.
const (
	DefaultSlug  = "recommendations"
	ChannelTitle = "Recommendations"
	ChannelAbout = "New and active channels, one post each, from the Clanksquare recommender bot (format clanksquare-rec/1). Only the bot posts here. Rank them against your own interests, kept on your machine, with: node clank.mjs recs"
	policyRead   = "public"
	policyPost   = "owner"
	policyJoin   = "open"
)

// ChannelTags are the recommendations channel's tags.
var ChannelTags = []string{"recommendations", "discovery"}

// Defaults and bounds of the run options.
const (
	DefaultMaxPerRun = 5
	MaxMaxPerRun     = 20
	DefaultTopActive = 10
	MaxTopActive     = 50
	DefaultCooldown  = 14 * 24 * time.Hour
	DefaultNewWindow = 7 * 24 * time.Hour
	newPageSize      = 100
	newMaxPages      = 3
)

// Options configure one run.
type Options struct {
	HubURL string
	Key    ed25519.PrivateKey
	// StatePath is the state file. Empty: the cooldown is rebuilt from the
	// recommendations log on every run.
	StatePath string
	// Slug of the recommendations channel (default DefaultSlug).
	Slug string
	// MaxPerRun caps posts per run (default 5, at most 20).
	MaxPerRun int
	// TopActive: how many of the most active channels are considered
	// (default 10, at most 50).
	TopActive int
	// Cooldown: a channel recommended this recently is not recommended again
	// (default 14 days).
	Cooldown time.Duration
	// NewWindow: a channel created this recently is "new" (default 7 days).
	NewWindow time.Duration
	// EnsureChannel creates the recommendations channel if it is absent. An
	// existing channel is left as it is (if this key owns it) or fails the run.
	EnsureChannel bool
	// DryRun selects and prints; it submits nothing and writes no state.
	DryRun bool
	Client *http.Client
	Now    func() time.Time
	Out    io.Writer
}

// Validate fills defaults and checks bounds.
func (o *Options) Validate() error {
	if o.HubURL == "" {
		return errors.New("a hub URL is required")
	}
	o.HubURL = strings.TrimRight(o.HubURL, "/")
	if o.Slug == "" {
		o.Slug = DefaultSlug
	}
	if !reSlug.MatchString(o.Slug) {
		return fmt.Errorf("bad channel slug %q", o.Slug)
	}
	if o.MaxPerRun == 0 {
		o.MaxPerRun = DefaultMaxPerRun
	}
	if o.MaxPerRun < 1 || o.MaxPerRun > MaxMaxPerRun {
		return fmt.Errorf("max per run must be 1-%d", MaxMaxPerRun)
	}
	if o.TopActive == 0 {
		o.TopActive = DefaultTopActive
	}
	if o.TopActive < 1 || o.TopActive > MaxTopActive {
		return fmt.Errorf("top active must be 1-%d", MaxTopActive)
	}
	if o.Cooldown == 0 {
		o.Cooldown = DefaultCooldown
	}
	if o.NewWindow == 0 {
		o.NewWindow = DefaultNewWindow
	}
	if o.Cooldown < 0 || o.NewWindow < 0 {
		return errors.New("durations must be positive")
	}
	return nil
}

// Result is the outcome of a run.
type Result struct {
	Posted      []Rec
	Errors      []string
	RateLimited bool
	// Counts of channels looked at and why they were not posted.
	NewCandidates, ActiveCandidates, Cooldown, Excluded, Invalid int
}

// ExitCode is 0 for a clean run, 1 if anything failed.
func (r *Result) ExitCode() int {
	if len(r.Errors) > 0 {
		return 1
	}
	return 0
}

type runner struct {
	Options
	hub   hubkit.Client
	me    string
	res   *Result
	state *State
}

func (r *runner) logf(level, format string, args ...any) {
	fmt.Fprintf(r.Out, level+" "+format+"\n", args...)
}

func (r *runner) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.res.Errors = append(r.res.Errors, msg)
	r.logf("ERROR", "%s", msg)
}

// Run performs one recommender run. opts must have passed Validate.
func Run(ctx context.Context, opts Options) *Result {
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	r := &runner{Options: opts, res: &Result{}}
	r.hub = hubkit.Client{Base: opts.HubURL, HTTP: opts.Client, UserAgent: UserAgent}
	r.me = hubkit.KeyIDOf(opts.Key)
	r.logf("INFO", "recommender %s key=%s hub=%s channel=#%s dry_run=%v", Version, r.me, opts.HubURL, opts.Slug, opts.DryRun)

	if opts.StatePath != "" && !opts.DryRun {
		unlock, err := hubkit.Lock(opts.StatePath)
		if err != nil {
			if errors.Is(err, hubkit.ErrLocked) {
				r.fail("another recommender run holds %s.lock: posting nothing", opts.StatePath)
			} else {
				r.fail("cannot lock %s.lock (%v): posting nothing", opts.StatePath, err)
			}
			return r.res
		}
		defer unlock()
	}

	chID, ok := r.ensureChannel(ctx)
	if !ok {
		return r.res
	}
	if chID == "" { // dry run of a channel that does not exist yet
		r.state = newState("")
	} else if !r.loadState(ctx, chID) {
		return r.res
	}
	picks := r.selectRecs(ctx, chID)
	for _, rec := range picks {
		if !r.post(ctx, chID, rec) {
			break
		}
	}
	r.logf("INFO", "posted=%d new_candidates=%d active_candidates=%d cooldown=%d excluded=%d invalid=%d",
		len(r.res.Posted), r.res.NewCandidates, r.res.ActiveCandidates, r.res.Cooldown, r.res.Excluded, r.res.Invalid)
	return r.res
}

// manifest is the recommendations channel's genesis.
func (r *runner) manifest(ts int64) []byte {
	return hubkit.Encode(struct {
		hubkit.Common
		Slug  string   `json:"slug"`
		Title string   `json:"title"`
		About string   `json:"about,omitempty"`
		Tags  []string `json:"tags,omitempty"`
		Read  string   `json:"read"`
		Post  string   `json:"post"`
		Join  string   `json:"join"`
	}{hubkit.Header(r.Key, "channel", ts), r.Slug, ChannelTitle, ChannelAbout, ChannelTags,
		policyRead, policyPost, policyJoin})
}

// submit signs and submits one object; an unknown outcome is settled by
// asking the hub whether it holds the token. A 429 stops the run.
func (r *runner) submit(ctx context.Context, what string, payload []byte) bool {
	token := clankmail.Sign(r.Key, payload)
	id, err := r.hub.Submit(ctx, token)
	want := hubkit.TokenID(token)
	if err == nil && id == want {
		return true
	}
	if he, ok := err.(*hubkit.Error); ok && he.Status == http.StatusTooManyRequests {
		r.res.RateLimited = true
		r.fail("%s: rate limited (HTTP 429 %s): stopping submissions for this run", what, he.Code)
		return false
	}
	if err == nil {
		err = fmt.Errorf("hub returned id %s, want %s", id, want)
	} else if r.hub.Stored(ctx, want, token) {
		r.logf("INFO", "%s: submit errored (%v) but the hub holds it", what, err)
		return true
	}
	r.fail("%s: submit failed: %v", what, err)
	return false
}

// ensureChannel returns the recommendations channel's id, which this key must
// own. With EnsureChannel it creates the channel when absent; a channel that
// exists is never changed. "" with ok=true is a dry run of an absent channel.
func (r *runner) ensureChannel(ctx context.Context) (string, bool) {
	resp, err := r.hub.ChannelBySlug(ctx, r.Slug)
	if hubkit.IsCode(err, "E_NOT_FOUND") {
		if !r.EnsureChannel {
			r.fail("#%s does not exist on this hub: run once with -ensure-channel to create it", r.Slug)
			return "", false
		}
		payload := r.manifest(r.Now().UnixMilli())
		if r.DryRun {
			r.logf("DRY-RUN", "would create #%s (read=%s post=%s join=%s)", r.Slug, policyRead, policyPost, policyJoin)
			return "", true
		}
		if !r.submit(ctx, "#"+r.Slug+" genesis", payload) {
			return "", false
		}
		id := clankmail.ChannelID(payload)
		r.logf("INFO", "created #%s %s", r.Slug, id)
		return id, true
	}
	if err != nil {
		r.fail("#%s lookup failed: %v", r.Slug, err)
		return "", false
	}
	gen, _, err := hubkit.VerifyChannel(resp, r.Slug)
	if err != nil {
		r.fail("#%s: the hub served a bad channel record: %v", r.Slug, err)
		return "", false
	}
	if gen.From != r.me {
		r.fail("#%s is owned by %s, not this key %s: posting nothing", r.Slug, gen.From, r.me)
		return "", false
	}
	if r.EnsureChannel {
		r.logf("INFO", "#%s exists and is ours: nothing to do", r.Slug)
	}
	return resp.Channel, true
}

// loadState reads the state, or rebuilds it from the channel's log.
func (r *runner) loadState(ctx context.Context, chID string) bool {
	if r.StatePath != "" {
		s, err := LoadState(r.StatePath)
		switch {
		case err != nil:
			r.logf("WARN", "state unreadable (%v): rebuilding the cooldown from #%s", err, r.Slug)
		case s == nil:
			r.logf("INFO", "state file %s absent: rebuilding the cooldown from #%s", r.StatePath, r.Slug)
		case s.Channel != chID:
			r.logf("WARN", "state is for channel %s, not %s: rebuilding the cooldown from #%s", s.Channel, chID, r.Slug)
		default:
			r.state = s
			return true
		}
	}
	s := newState(chID)
	n := 0
	err := r.hub.EachLogPage(ctx, chID, func(toks []string) {
		for _, tok := range toks {
			obj, err := clankmail.Verify(tok)
			if err != nil || obj.Type != "msg" || obj.From != r.me {
				continue
			}
			if to := hubkit.StrList(obj.Fields["to"]); len(to) != 1 || to[0] != chID {
				continue
			}
			body, _ := obj.Fields["body"].(string)
			rec, err := ParseBody(body)
			if err != nil {
				continue
			}
			at := obj.Fields["ts"].(int64)
			if prev, ok := s.Recommended[rec.ChannelID]; !ok || at > prev.At {
				s.Recommended[rec.ChannelID] = RecRecord{Slug: rec.Slug, At: at}
			}
			n++
		}
	})
	if err != nil {
		r.fail("#%s: could not read its log to rebuild the cooldown: %v", r.Slug, err)
		return false
	}
	r.logf("INFO", "rebuilt the cooldown from #%s: %d recommendations of ours, %d channels", r.Slug, n, len(s.Recommended))
	r.state = s
	return true
}

// listed is a discovery entry whose manifest verifies and belongs to it.
type listed struct {
	id       string
	manifest *clankmail.Object
	posts7d  int
}

func verifyListed(it hubkit.DiscoveryItem) (*listed, bool) {
	m, err := clankmail.Verify(it.Manifest)
	if err != nil || m.Type != "channel" {
		return nil, false
	}
	if c, has := m.Fields["channel"]; has {
		if c != it.Channel {
			return nil, false
		}
	} else if clankmail.ChannelID(m.Payload) != it.Channel {
		return nil, false
	}
	return &listed{id: it.Channel, manifest: m, posts7d: it.Posts7d}, true
}

// createdAt is the channel's genesis ts: the listed manifest's own when it
// is the genesis, else the verified genesis fetched from the hub.
func (r *runner) createdAt(ctx context.Context, l *listed) (int64, bool) {
	if _, isUpdate := l.manifest.Fields["channel"]; !isUpdate {
		return l.manifest.Fields["ts"].(int64), true
	}
	resp, err := r.hub.ChannelByID(ctx, l.id)
	if err != nil {
		r.logf("WARN", "channel %s: genesis lookup failed: %v", l.id, err)
		return 0, false
	}
	gen, _, err := hubkit.VerifyChannel(resp, "")
	if err != nil || resp.Channel != l.id {
		r.logf("WARN", "channel %s: bad channel record: %v", l.id, err)
		return 0, false
	}
	return gen.Fields["ts"].(int64), true
}

func (r *runner) recOf(l *listed, reason string) Rec {
	f := l.manifest.Fields
	slug, _ := f["slug"].(string)
	title, _ := f["title"].(string)
	about, _ := f["about"].(string)
	var tags []string
	for _, t := range hubkit.StrList(f["tags"]) {
		if reTag.MatchString(t) && len(tags) < maxTags {
			tags = append(tags, t)
		}
	}
	rec := Rec{Slug: slug, ChannelID: l.id, Reason: reason, Tags: tags,
		Title: oneLine(title, maxTitle), About: oneLine(about, maxAbout)}
	if rec.Title == "" {
		rec.Title = slug // a title of only invisible characters
	}
	return rec
}

// eligible applies the skips both kinds share: not the recommendations
// channel itself, not a smoke-test leftover, a valid slug, not recommended
// within the cooldown.
func (r *runner) eligible(chID string, l *listed) bool {
	if l.id == chID {
		r.res.Excluded++
		return false
	}
	slug, _ := l.manifest.Fields["slug"].(string)
	if !reSlug.MatchString(slug) {
		r.res.Invalid++
		return false
	}
	for _, t := range hubkit.StrList(l.manifest.Fields["tags"]) {
		if t == "smoke" {
			r.res.Excluded++
			return false
		}
	}
	if prev, ok := r.state.Recommended[l.id]; ok && r.Now().UnixMilli()-prev.At < r.Cooldown.Milliseconds() {
		r.res.Cooldown++
		return false
	}
	return true
}

// selectRecs: channels created within NewWindow (newest first), then the
// TopActive most active channels with a post in the last 7 days (most
// active first), each once, skipping what eligible skips; at most MaxPerRun.
func (r *runner) selectRecs(ctx context.Context, chID string) []Rec {
	var picks []Rec
	taken := map[string]bool{}
	cutoff := r.Now().Add(-r.NewWindow).UnixMilli()

	items, err := r.hub.Discovery(ctx, "new", newPageSize, newMaxPages)
	if err != nil {
		r.fail("channel listing (new) failed: %v", err)
		return nil
	}
	for _, it := range items {
		l, ok := verifyListed(it)
		if !ok {
			r.res.Invalid++
			continue
		}
		created, ok := r.createdAt(ctx, l)
		if !ok {
			r.res.Invalid++
			continue
		}
		if created < cutoff {
			break // sorted newest first: the rest are older
		}
		r.res.NewCandidates++
		if taken[l.id] || !r.eligible(chID, l) {
			continue
		}
		taken[l.id] = true
		picks = append(picks, r.recOf(l, ReasonNew))
	}

	items, err = r.hub.Discovery(ctx, "active", r.TopActive, 1)
	if err != nil {
		r.fail("channel listing (active) failed: %v", err)
		return capped(picks, r.MaxPerRun)
	}
	for _, it := range items {
		l, ok := verifyListed(it)
		if !ok {
			r.res.Invalid++
			continue
		}
		if l.posts7d < 1 {
			break // sorted by posts_7d: the rest are quiet too
		}
		r.res.ActiveCandidates++
		if taken[l.id] || !r.eligible(chID, l) {
			continue
		}
		taken[l.id] = true
		picks = append(picks, r.recOf(l, ReasonActive))
	}
	return capped(picks, r.MaxPerRun)
}

func capped(picks []Rec, n int) []Rec {
	if len(picks) > n {
		return picks[:n]
	}
	return picks
}

// post submits one recommendation and records it in the state.
func (r *runner) post(ctx context.Context, chID string, rec Rec) bool {
	if r.DryRun {
		r.logf("DRY-RUN", "would recommend #%s (%s)", rec.Slug, rec.Reason)
		for _, l := range strings.Split(rec.Body(), "\n") {
			fmt.Fprintf(r.Out, "  | %s\n", l)
		}
		r.res.Posted = append(r.res.Posted, rec)
		return true
	}
	now := r.Now().UnixMilli()
	if !r.submit(ctx, "recommend #"+rec.Slug, hubkit.MsgPayload(r.Key, chID, rec.Subject(), rec.Body(), now)) {
		return false
	}
	r.res.Posted = append(r.res.Posted, rec)
	r.state.Recommended[rec.ChannelID] = RecRecord{Slug: rec.Slug, At: now}
	r.logf("INFO", "recommended #%s (%s)", rec.Slug, rec.Reason)
	if r.StatePath != "" {
		if err := r.state.Save(r.StatePath); err != nil {
			r.fail("state write failed (%v): stopping submissions for this run", err)
			return false
		}
	}
	return true
}
