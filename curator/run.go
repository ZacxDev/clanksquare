// Package curator reposts new items from public RSS/Atom feeds into ClankMail
// topic channels, signed with the curator's own key. Selection and formatting
// are deterministic functions of the feeds, the config, the seen set and the
// clock; no model is involved.
package curator

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Version is reported in the User-Agent.
const Version = "0.1.0"

// UserAgent is sent on every feed and hub request.
const UserAgent = "clankmail-curator/" + Version + " (+https://github.com/ZacxDev/clankmail)"

const (
	maxSubject = 200
	maxExcerpt = 500
	maxVia     = 120
	// futureSlack: an item dated further ahead than this has no trustworthy
	// date (it would otherwise rank newest forever) and is treated as undated.
	futureSlack = 24 * time.Hour
)

// Options configure one run.
type Options struct {
	Config *Config
	Key    ed25519.PrivateKey
	// StatePath is the state file. Empty: no state is kept, and every
	// channel's seen set is rebuilt from its log each run.
	StatePath string
	// DryRun fetches and selects, prints what would be posted, and submits
	// nothing and writes nothing.
	DryRun bool
	// Client is used for feeds and the hub; nil means a default client.
	Client *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Out receives the log and the summary lines; nil means os.Stdout.
	Out io.Writer

	// afterPost, when set (tests only), runs after each successful post and
	// its state write. A panic there simulates a crash mid-run.
	afterPost func(key string)
}

// ChannelStats is one channel's summary for a run.
type ChannelStats struct {
	Slug        string
	FeedsOK     int
	FeedsFailed int
	Items       int // items in feeds that were fetched
	New         int // candidates left after every skip below
	Posted      int
	Seen        int // already posted (state or log), or a duplicate within the run
	TooOld      int
	Undated     int // no date, or one too far in the future
	Filtered    int // include/exclude, or no usable link or guid
}

func (s ChannelStats) String() string {
	return fmt.Sprintf("channel=%s feeds_ok=%d feeds_failed=%d items=%d new=%d posted=%d seen=%d too_old=%d undated=%d filtered=%d",
		s.Slug, s.FeedsOK, s.FeedsFailed, s.Items, s.New, s.Posted, s.Seen, s.TooOld, s.Undated, s.Filtered)
}

// Result is the outcome of a run.
type Result struct {
	Channels    []ChannelStats
	Errors      []string // each one makes the run fail
	FeedsOK     int
	FeedsFailed int
	RateLimited bool
}

// ExitCode is 0 for a clean run and 1 if anything the run is accountable for
// failed: an error was recorded, or no feed at all could be read.
func (r *Result) ExitCode() int {
	if len(r.Errors) > 0 || r.FeedsOK == 0 {
		return 1
	}
	return 0
}

type runner struct {
	Options
	hub     hubClient
	me      string
	state   *State
	res     *Result
	stopped bool // E_RATE (or a state write failure): submit nothing more
	// stateBroken: a state write failed and the file was removed; writing a
	// state that lacks a post would be worse than writing none.
	stateBroken bool
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

// Run performs one curator run.
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
	r.hub = hubClient{base: opts.Config.HubURL, client: opts.Client}
	r.me = clankmail.KeyID(opts.Key.Public().(ed25519.PublicKey))
	r.logf("INFO", "curator %s key=%s hub=%s dry_run=%v", Version, r.me, opts.Config.HubURL, opts.DryRun)

	r.loadState()
	if opts.Config.Handle != "" {
		r.ensureHandle(ctx)
	}
	for i := range opts.Config.Channels {
		if r.stopped {
			r.logf("WARN", "channel=%s skipped: submissions stopped for this run", opts.Config.Channels[i].Slug)
			r.res.Channels = append(r.res.Channels, ChannelStats{Slug: opts.Config.Channels[i].Slug})
			continue
		}
		r.runChannel(ctx, &opts.Config.Channels[i])
	}
	// Persist what this run learned (rebuilt seen sets, pruning) even when it
	// posted nothing, so the next run does not read every log again.
	if !r.stateBroken {
		r.saveState()
	}
	if r.res.FeedsOK == 0 {
		r.logf("ERROR", "no feed could be read this run")
	}
	for _, s := range r.res.Channels {
		fmt.Fprintln(r.Out, s.String())
	}
	return r.res
}

func (r *runner) loadState() {
	if r.StatePath == "" {
		r.state = newState()
		r.logf("INFO", "no state file: seen sets come from the channel logs")
		return
	}
	s, err := LoadState(r.StatePath)
	switch {
	case err != nil:
		r.logf("WARN", "state unreadable (%v): rebuilding every channel from its log", err)
		s = newState()
	case s == nil:
		r.logf("INFO", "state file %s absent: rebuilding every channel from its log", r.StatePath)
		s = newState()
	}
	s.prune(r.nowMS() - seenTTLHours*3600*1000)
	r.state = s
}

// saveState persists the state after a post. If it cannot be written, the
// state file is removed so the next run rebuilds from the log instead of
// trusting a file that lacks this post, and submitting stops.
func (r *runner) saveState() {
	if r.DryRun || r.StatePath == "" {
		return
	}
	if err := r.state.Save(r.StatePath); err != nil {
		r.fail("state write failed (%v): stopping; removing %s so the next run rebuilds from the log", err, r.StatePath)
		os.Remove(r.StatePath)
		r.stopped = true
		r.stateBroken = true
	}
}

// submit signs payload, submits it, and checks the hub's answer is the
// payload's own id. A network error or 5xx is resolved by asking the hub
// whether it stored the token, so an ambiguous failure is never retried
// into a second post next run. E_RATE stops the run's submissions.
func (r *runner) submit(ctx context.Context, what string, payload []byte) (string, bool) {
	token := clankmail.Sign(r.Key, payload)
	want := clankmail.ObjectID(payload)
	id, err := r.hub.submit(ctx, token)
	if err != nil {
		he, isHub := err.(*HubError)
		if isHub && he.Code == "E_RATE" {
			r.fail("%s: rate limited (E_RATE): stopping submissions for this run", what)
			r.res.RateLimited = true
			r.stopped = true
			return "", false
		}
		if (!isHub || he.Status >= 500) && r.hub.stored(ctx, want, token) {
			r.logf("INFO", "%s: submit errored (%v) but the hub holds %s", what, err, want)
			return want, true
		}
		r.fail("%s: submit failed: %v", what, err)
		return "", false
	}
	if id != want {
		r.fail("%s: hub returned id %s, want %s", what, id, want)
		return "", false
	}
	return id, true
}

func (r *runner) ensureHandle(ctx context.Context) {
	name := r.Config.Handle
	var resp struct {
		Handle string `json:"handle"`
	}
	err := r.hub.get(ctx, "/v1/handles/"+name, &resp)
	switch {
	case isCode(err, "E_NOT_FOUND"):
		if r.DryRun {
			r.logf("DRY-RUN", "would claim handle @%s", name)
			return
		}
		if _, ok := r.submit(ctx, "handle @"+name, handlePayload(r.Key, name, r.Config.Hub, r.nowMS())); ok {
			r.logf("INFO", "claimed handle @%s", name)
		}
		return
	case err != nil:
		r.fail("handle @%s: lookup failed: %v", name, err)
		return
	}
	obj, verr := clankmail.Verify(resp.Handle)
	if verr != nil || obj.Type != "handle" || obj.Fields["name"] != name {
		r.fail("handle @%s: hub served an invalid handle token (%v)", name, verr)
		return
	}
	if obj.From != r.me {
		r.fail("handle @%s is held by another key (%s)", name, obj.From)
		return
	}
	r.logf("INFO", "handle @%s already bound to this key", name)
}

// ensureChannel returns the channel id for ch, creating or updating the
// channel as needed. "" with ok=true means a dry run of a channel that does
// not exist yet.
func (r *runner) ensureChannel(ctx context.Context, ch *Channel) (string, bool) {
	resp, err := r.hub.channelBySlug(ctx, ch.Slug)
	if isCode(err, "E_NOT_FOUND") {
		payload := ch.manifest(r.Key, "", r.nowMS())
		if r.DryRun {
			r.logf("DRY-RUN", "channel=%s would create channel", ch.Slug)
			return "", true
		}
		if _, ok := r.submit(ctx, "channel #"+ch.Slug+" genesis", payload); !ok {
			r.fail("channel=%s could not be created", ch.Slug)
			return "", false
		}
		id := clankmail.ChannelID(payload)
		r.logf("INFO", "channel=%s created %s", ch.Slug, id)
		return id, true
	}
	if err != nil {
		r.fail("channel=%s lookup failed: %v", ch.Slug, err)
		return "", false
	}
	// §4: clients MUST verify what the hub serves before trusting it.
	gen, gerr := clankmail.Verify(resp.Genesis)
	man, merr := clankmail.Verify(resp.Manifest)
	switch {
	case gerr != nil || merr != nil:
		r.fail("channel=%s hub served an unverifiable manifest (genesis: %v, manifest: %v)", ch.Slug, gerr, merr)
		return "", false
	case gen.Type != "channel" || gen.Fields["channel"] != nil || clankmail.ChannelID(gen.Payload) != resp.Channel:
		r.fail("channel=%s genesis does not match channel id %s", ch.Slug, resp.Channel)
		return "", false
	case man.Type != "channel" || man.From != gen.From ||
		man.ID != gen.ID && man.Fields["channel"] != resp.Channel || man.Fields["slug"] != ch.Slug:
		r.fail("channel=%s manifest is not a valid manifest of %s", ch.Slug, resp.Channel)
		return "", false
	}
	if gen.From != r.me {
		r.fail("channel=%s is SQUATTED: #%s is owned by %s, not this key %s; skipping it", ch.Slug, ch.Slug, gen.From, r.me)
		return "", false
	}
	if ch.manifestDiffers(man.Fields) {
		ts := max(r.nowMS(), man.Fields["ts"].(int64)+1)
		payload := ch.manifest(r.Key, resp.Channel, ts, strList(man.Fields["mods"])...)
		if r.DryRun {
			r.logf("DRY-RUN", "channel=%s manifest differs from config: would update", ch.Slug)
			return resp.Channel, true
		}
		if _, ok := r.submit(ctx, "channel #"+ch.Slug+" update", payload); !ok {
			r.fail("channel=%s could not be updated", ch.Slug)
			return "", false
		}
		r.logf("INFO", "channel=%s manifest updated", ch.Slug)
	}
	return resp.Channel, true
}

// rebuildSeen derives a channel's seen set from its log: every post that
// verifies, is a msg to this channel, and is signed by this key, keyed by the
// first line of its body (always the link, see formatPost).
func (r *runner) rebuildSeen(ctx context.Context, channelID string) (map[string]int64, error) {
	toks, err := r.hub.logTokens(ctx, channelID)
	if err != nil {
		return nil, err
	}
	seen := map[string]int64{}
	ignored := 0
	for _, tok := range toks {
		obj, err := clankmail.Verify(tok)
		if err != nil || obj.Type != "msg" || obj.From != r.me {
			ignored++
			continue
		}
		to := strList(obj.Fields["to"])
		if len(to) != 1 || to[0] != channelID {
			ignored++
			continue
		}
		body, _ := obj.Fields["body"].(string)
		line, _, _ := strings.Cut(body, "\n")
		key := keyFromLine(line)
		if key == "" {
			ignored++
			continue
		}
		at := obj.Fields["ts"].(int64)
		if prev, ok := seen[key]; !ok || at < prev {
			seen[key] = at
		}
	}
	r.logf("INFO", "rebuilt seen set for %s from its log: %d posts of ours, %d other log entries ignored", channelID, len(seen), ignored)
	return seen, nil
}

func (r *runner) runChannel(ctx context.Context, ch *Channel) {
	stats := ChannelStats{Slug: ch.Slug}
	defer func() { r.res.Channels = append(r.res.Channels, stats) }()

	id, ok := r.ensureChannel(ctx, ch)
	if !ok {
		if !r.DryRun {
			return
		}
		// A dry run still evaluates the feeds (the error above already
		// fails the run), as if the channel were new and empty.
		id = ""
	}
	var seen map[string]int64
	if cs := r.state.Channels[id]; id != "" && cs != nil {
		seen = cs.Seen
	} else if id != "" {
		s, err := r.rebuildSeen(ctx, id)
		if err != nil {
			r.fail("channel=%s could not read its log to rebuild the seen set: %v", ch.Slug, err)
			return
		}
		seen = s
		r.state.Channels[id] = &ChannelState{Slug: ch.Slug, Seen: seen}
		r.state.prune(r.nowMS() - seenTTLHours*3600*1000)
	} else {
		seen = map[string]int64{} // dry run of a channel that does not exist yet
	}

	var fetched []fetchedFeed
	for _, fc := range ch.Feeds {
		f, err := FetchFeed(ctx, r.Client, fc.URL)
		if err != nil {
			stats.FeedsFailed++
			r.res.FeedsFailed++
			r.logf("WARN", "channel=%s feed %s failed: %v", ch.Slug, fc.URL, err)
			continue
		}
		stats.FeedsOK++
		r.res.FeedsOK++
		r.logf("INFO", "channel=%s feed %s ok items=%d", ch.Slug, fc.URL, len(f.Items))
		fetched = append(fetched, fetchedFeed{fc, f})
	}

	picked := selectItems(ch, fetched, seen, r.Now(), &stats)
	for _, c := range picked {
		subject, body := formatPost(c)
		if r.DryRun {
			r.logf("DRY-RUN", "channel=%s would post %q %s", ch.Slug, subject, c.line)
			for _, l := range strings.Split(body, "\n") {
				fmt.Fprintf(r.Out, "  | %s\n", l)
			}
			stats.Posted++
			continue
		}
		if r.stopped {
			return
		}
		payload := msgPayload(r.Key, id, subject, body, r.nowMS())
		if _, ok := r.submit(ctx, "channel="+ch.Slug+" post "+c.line, payload); !ok {
			continue
		}
		stats.Posted++
		seen[c.key] = r.nowMS()
		r.saveState()
		if r.afterPost != nil {
			r.afterPost(c.key)
		}
	}
}

// ---- selection ----

type fetchedFeed struct {
	cfg  FeedConfig
	feed *Feed
}

type candidate struct {
	key     string // dedupe key: the normalised link, or "guid:" + guid
	line    string // first body line: the normalised link, or the guid
	title   string
	summary string
	via     string
	date    time.Time
}

// keyFromLine maps a post's first body line back to its dedupe key. It is
// the inverse formatPost and selectItems rely on: a line that is a link
// normalises to itself; anything else was a guid.
func keyFromLine(line string) string {
	if line == "" {
		return ""
	}
	if l := NormalizeLink(line); l != "" {
		return l
	}
	return "guid:" + line
}

// selectItems applies the per-item skips in a fixed order (undated, too old,
// no usable link, filters, already seen), dedupes across feeds (the first
// feed in config order wins), then takes the newest MaxPerRun (equal dates: lower key
// first) and returns them oldest first (equal dates in key order), so the
// result is a function of its inputs alone.
func selectItems(ch *Channel, feeds []fetchedFeed, seen map[string]int64, now time.Time, st *ChannelStats) []candidate {
	cutoff := now.Add(-time.Duration(ch.MaxAgeHours) * time.Hour)
	inRun := map[string]bool{}
	var cands []candidate
	for _, ff := range feeds {
		via := Truncate(PlainText(ff.cfg.Name), maxVia)
		if via == "" {
			via = Truncate(ff.feed.Title, maxVia)
		}
		if via == "" {
			via = hostOf(ff.cfg.URL)
		}
		for _, it := range ff.feed.Items {
			st.Items++
			switch {
			case it.Date.IsZero() || it.Date.After(now.Add(futureSlack)):
				st.Undated++
				continue
			case it.Date.Before(cutoff):
				st.TooOld++
				continue
			}
			line := NormalizeLink(it.Link)
			if line == "" {
				line = NormalizeLink(it.GUID)
			}
			key := line
			if line == "" {
				if g := clip(PlainText(it.GUID), maxLink); g != "" {
					line, key = g, "guid:"+g
				}
			}
			text := it.Title + "\n" + it.Summary
			if key == "" ||
				len(ff.cfg.Include) > 0 && !MatchesKeyword(text, ff.cfg.Include) ||
				MatchesKeyword(text, ff.cfg.Exclude) {
				st.Filtered++
				continue
			}
			if _, ok := seen[key]; ok || inRun[key] {
				st.Seen++
				continue
			}
			inRun[key] = true
			cands = append(cands, candidate{key: key, line: line, title: it.Title, summary: it.Summary, via: via, date: it.Date})
		}
	}
	st.New = len(cands)
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].date.Equal(cands[j].date) {
			return cands[i].date.After(cands[j].date)
		}
		return cands[i].key < cands[j].key
	})
	if len(cands) > ch.MaxPerRun {
		cands = cands[:ch.MaxPerRun]
	}
	// Post oldest first; equal dates in key order.
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].date.Equal(cands[j].date) {
			return cands[i].date.Before(cands[j].date)
		}
		return cands[i].key < cands[j].key
	})
	return cands
}

// formatPost renders a candidate. The body's first line is always c.line;
// rebuildSeen depends on it.
//
//	<link>
//
//	<excerpt, ≤ 500 bytes>   (omitted when empty)
//
//	via <feed title or host>
func formatPost(c candidate) (subject, body string) {
	subject = Truncate(c.title, maxSubject)
	if subject == "" {
		subject = Truncate(hostOf(c.line), maxSubject)
	}
	parts := []string{c.line}
	if ex := Truncate(c.summary, maxExcerpt); ex != "" {
		parts = append(parts, ex)
	}
	parts = append(parts, "via "+c.via)
	return subject, strings.Join(parts, "\n\n")
}
