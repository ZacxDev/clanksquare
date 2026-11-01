// Package curator reposts new items from public RSS/Atom feeds into ClankMail
// topic channels, signed with the curator's own key. Selection and formatting
// are deterministic functions of the feeds, the config, the seen set and the
// clock; no model is involved.
package curator

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
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
	stopped bool // a 429 or a state write failure: submit nothing more
	// blocked holds channels with a pending post the hub could not confirm
	// or refuse: nothing new is posted to them this run.
	blocked map[string]bool
	// tried holds pending keys already resent this run.
	tried map[string]bool
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
	r := &runner{Options: opts, res: &Result{}, blocked: map[string]bool{}, tried: map[string]bool{}}
	r.hub = hubClient{base: opts.Config.HubURL, client: opts.Client}
	r.me = clankmail.KeyID(opts.Key.Public().(ed25519.PublicKey))
	r.logf("INFO", "curator %s key=%s hub=%s dry_run=%v", Version, r.me, opts.Config.HubURL, opts.DryRun)

	// Two overlapping runs would both post the same items. Without a state
	// file there is nothing to lock: overlapping runs must then be
	// prevented by the schedule.
	if opts.StatePath != "" && !opts.DryRun {
		unlock, err := lockState(opts.StatePath)
		if err != nil {
			if errors.Is(err, errLocked) {
				r.fail("another curator run holds %s.lock: posting nothing", opts.StatePath)
			} else {
				r.fail("cannot lock %s.lock (%v): posting nothing", opts.StatePath, err)
			}
			return r.res
		}
		defer unlock()
	}

	r.loadState()
	r.resolveAll()
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
	// Resolve what this run left unconfirmed, then persist what it learned
	// (rebuilt seen sets) even when it posted nothing, so the next run does
	// not read every log again.
	r.resolveAll()
	r.saveState()
	for id, cs := range r.state.Channels {
		if len(cs.Pending) > 0 {
			r.fail("channel=%s (%s): %d post(s) the hub has neither confirmed nor refused; they are resent next run and nothing new is posted there until then", cs.Slug, id, len(cs.Pending))
		}
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
	// Seen keys are never forgotten by age: a feed can re-date an old item
	// (GitHub's releases.atom carries only <updated>, which moves when a
	// release is edited), and a forgotten key would then post again.
	r.state = s
}

// saveState persists the state. If it cannot be written, submitting stops
// and the run fails. The file on disk stays as it was, which is safe: every
// post on it is either seen or pending (written before its submit), and a
// pending post is resolved by resending its exact token next run.
func (r *runner) saveState() bool {
	if r.DryRun || r.StatePath == "" {
		return true
	}
	if err := r.state.Save(r.StatePath); err != nil {
		r.fail("state write failed (%v): stopping submissions for this run", err)
		r.stopped = true
		return false
	}
	return true
}

// outcome is what a submission established about a token.
type outcome int

const (
	confirmed outcome = iota // the hub holds it (200 with its id)
	refused                  // one of notStoredCodes: the hub does not hold it
	unknown                  // anything else: it may be stored
)

// notStoredCodes are the only answers that settle a token as NOT stored, so
// its pending record may be dropped (and the item posted again, once). Each
// is decided by the hub after its "id already stored" check (SPEC §5; go/hub
// submit), so a stored token can never get one, and none can turn into a
// success for these exact bytes later:
//
//   - E_STALE: ts is outside ±300 s; our tokens only grow older.
//   - E_NOT_FOUND: the channel the post names does not exist (§7.2).
//
// Everything else is unknown: a 4xx without a ClankMail code (a proxy's
// "404 page not found"), the §4 codes (a truncated or mangled body fails
// verification before the stored-id check), E_FORBIDDEN and E_REVOKED
// (left out on purpose: they fail loud instead; a resend after five minutes
// turns into E_STALE anyway), any 5xx, 429 or network error.
var notStoredCodes = map[string]int{"E_STALE": http.StatusBadRequest, "E_NOT_FOUND": http.StatusNotFound}

// send submits token and classifies the answer: confirmed (200 with the
// token's id), refused (notStoredCodes) or unknown. A 429 also stops
// submitting for the run, whether or not it carries E_RATE (a relay's 429
// does not).
func (r *runner) send(ctx context.Context, what, token string) (outcome, error) {
	want := tokenID(token)
	id, err := r.hub.submit(ctx, token)
	if err == nil {
		if id != want {
			return unknown, fmt.Errorf("%w: hub returned id %s, want %s", errWrongID, id, want)
		}
		return confirmed, nil
	}
	if he, ok := err.(*HubError); ok {
		switch {
		case he.Status == http.StatusTooManyRequests:
			if !r.stopped {
				r.fail("%s: rate limited (HTTP 429 %s): stopping submissions for this run", what, he.Code)
			}
			r.res.RateLimited = true
			r.stopped = true
			return unknown, err
		case he.Code != "" && notStoredCodes[he.Code] == he.Status:
			return refused, err
		}
	}
	return unknown, err
}

// errWrongID marks a 200 whose id is not the token's: the hub misbehaves,
// and asking it whether it holds the token settles nothing.
var errWrongID = errors.New("wrong id")

// tokenID is the m_ id of one of our own tokens.
func tokenID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ""
	}
	return clankmail.ObjectID(payload)
}

// submit signs and submits a handle or channel object. These are not
// tracked as pending: a duplicate of either is refused or harmless (a slug
// binds once, a handle names one key). An unknown outcome is settled by
// asking the hub whether it holds the token.
func (r *runner) submit(ctx context.Context, what string, payload []byte) (string, bool) {
	token := clankmail.Sign(r.Key, payload)
	out, err := r.send(ctx, what, token)
	switch {
	case out == confirmed:
		return tokenID(token), true
	case out == unknown && !r.res.RateLimited && !errors.Is(err, errWrongID) && r.hub.stored(ctx, tokenID(token), token):
		r.logf("INFO", "%s: submit errored (%v) but the hub holds it", what, err)
		return tokenID(token), true
	case !r.res.RateLimited:
		r.fail("%s: submit failed: %v", what, err)
	}
	return "", false
}

// resolveTimeout bounds one resend of a pending token.
const resolveTimeout = 15 * time.Second

// resolveAll resends every pending token not yet tried this run, under a
// fresh context: the run's own may be cancelled (SIGTERM), and a pending post
// must still be settled. Confirmed → seen; refused → dropped (it was never
// stored, so the item may legitimately post once later); unknown → kept,
// and its channel posts nothing new this run.
func (r *runner) resolveAll() {
	if r.DryRun {
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
			if r.tried[id+" "+k] {
				r.blocked[id] = true
				continue
			}
			if r.stopped {
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
				r.logf("WARN", "channel=%s pending post %s was not stored (%v): dropped; the item may post again", cs.Slug, k, err)
			default:
				r.blocked[id] = true
				r.logf("WARN", "channel=%s pending post %s still unconfirmed (%v)", cs.Slug, k, err)
			}
			r.saveState()
		}
	}
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
	seen := map[string]int64{}
	ignored := 0
	// Verified page by page; only the keys are kept.
	err := r.hub.eachLogPage(ctx, channelID, func(toks []string) {
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
	})
	if err != nil {
		return nil, err
	}
	r.logf("INFO", "rebuilt seen set for %s from its log: %d posts of ours, %d other log entries ignored", channelID, len(seen), ignored)
	return seen, nil
}

func (r *runner) runChannel(ctx context.Context, ch *Channel) {
	stats := ChannelStats{Slug: ch.Slug}
	defer func() { r.res.Channels = append(r.res.Channels, stats) }()

	id, ok := r.ensureChannel(ctx, ch)
	if ok && r.blocked[id] {
		r.fail("channel=%s has a post the hub has not confirmed: posting nothing new to it this run", ch.Slug)
	}
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
		if r.stopped || r.blocked[id] {
			return
		}
		cs := r.state.Channels[id]
		token := clankmail.Sign(r.Key, msgPayload(r.Key, id, subject, body, r.nowMS()))
		// Write-ahead: the token is on disk before the hub can see it.
		if cs.Pending == nil {
			cs.Pending = map[string]string{}
		}
		cs.Pending[c.key] = token
		if !r.saveState() {
			delete(cs.Pending, c.key) // never submitted
			return
		}
		what := "channel=" + ch.Slug + " post " + c.line
		out, err := r.send(ctx, what, token)
		switch out {
		case confirmed:
			delete(cs.Pending, c.key)
			seen[c.key] = r.nowMS()
			stats.Posted++
			if !r.saveState() {
				return // the pending record on disk settles it next run
			}
			if r.afterPost != nil {
				r.afterPost(c.key)
			}
		case refused:
			delete(cs.Pending, c.key)
			r.fail("%s: submit refused: %v", what, err)
			r.saveState()
		default:
			// Stored or not, we cannot tell yet: keep the pending record,
			// resend it at the end of the run, post nothing more here.
			r.blocked[id] = true
			r.logf("WARN", "%s: unconfirmed (%v): will resend the same token", what, err)
			return
		}
	}
}

// ---- selection ----

type fetchedFeed struct {
	cfg  FeedConfig
	feed *Feed
}

type candidate struct {
	key     string // dedupe key: always keyFromLine(line)
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
			if line == "" {
				line = clip(PlainText(it.GUID), maxLink)
			}
			// The key is derived from the exact first line that will be
			// posted, by the same function the log rebuild uses, so the two
			// can never disagree.
			key := keyFromLine(line)
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
