package recommender

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

const day = 24 * time.Hour

// t0 is the tests' clock: 2030-03-17T17:46:40Z.
var t0 = time.UnixMilli(1_900_000_000_000).UTC()

type world struct {
	t     *testing.T
	hub   *fakeHub
	bot   ed25519.PrivateKey
	other ed25519.PrivateKey
	dir   string
	now   time.Time
	out   bytes.Buffer
}

func newWorld(t *testing.T) *world {
	return &world{t: t, hub: newFakeHub(t), bot: testKey("bot"), other: testKey("someone"), dir: t.TempDir(), now: t0}
}

func (w *world) statePath() string { return filepath.Join(w.dir, "state.json") }

func (w *world) run(mod func(*Options)) *Result {
	w.t.Helper()
	w.out.Reset()
	o := Options{HubURL: w.hub.srv.URL, Key: w.bot, StatePath: w.statePath(), EnsureChannel: true,
		Now: func() time.Time { return w.now }, Out: &w.out}
	if mod != nil {
		mod(&o)
	}
	if err := o.Validate(); err != nil {
		w.t.Fatal(err)
	}
	return Run(context.Background(), o)
}

func (w *world) ms(d time.Duration) int64 { return w.now.Add(d).UnixMilli() }

func slugs(recs []Rec) string {
	var s []string
	for _, r := range recs {
		s = append(s, r.Slug+"/"+r.Reason)
	}
	return strings.Join(s, " ")
}

func TestEnsureChannelCreatesOnceWithOwnerPolicy(t *testing.T) {
	w := newWorld(t)
	if res := w.run(nil); res.ExitCode() != 0 {
		t.Fatalf("first run failed:\n%s", w.out.String())
	}
	if n := w.hub.count("channel"); n != 1 {
		t.Fatalf("%d channel objects after the first run, want 1", n)
	}
	id := w.hub.slugs[DefaultSlug]
	man, err := clankmail.Verify(w.hub.channels[id].manifest)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"read": "public", "post": "owner", "join": "open", "title": ChannelTitle, "about": ChannelAbout} {
		if man.Fields[k] != want {
			t.Errorf("manifest %s = %v, want %q", k, man.Fields[k], want)
		}
	}
	if man.From != keyID(w.bot) {
		t.Errorf("the channel is owned by %s, want the bot", man.From)
	}
	// Idempotent: a second ensure submits no channel object.
	w.now = w.now.Add(time.Hour)
	if res := w.run(nil); res.ExitCode() != 0 {
		t.Fatalf("second run failed:\n%s", w.out.String())
	}
	if n := w.hub.count("channel"); n != 1 {
		t.Fatalf("%d channel objects after a second -ensure-channel run, want 1 (not idempotent)", n)
	}
	if !strings.Contains(w.out.String(), "exists and is ours: nothing to do") {
		t.Errorf("the second run should say the channel is already right:\n%s", w.out.String())
	}
}

// An existing channel of ours is left alone, even if its manifest differs
// from the one -ensure-channel would create.
func TestEnsureChannelLeavesAnExistingChannelAlone(t *testing.T) {
	w := newWorld(t)
	id := w.hub.addChannel(w.bot, DefaultSlug, "Old title", "", nil, w.ms(-day), "owner")
	before := w.hub.channels[id].manifest
	if res := w.run(nil); res.ExitCode() != 0 {
		t.Fatalf("run failed:\n%s", w.out.String())
	}
	if w.hub.count("channel") != 1 || w.hub.channels[id].manifest != before {
		t.Fatalf("the existing channel was changed (%d channel objects)", w.hub.count("channel"))
	}
	if !strings.Contains(w.out.String(), "exists and is ours: nothing to do") {
		t.Errorf("should say it left the channel alone:\n%s", w.out.String())
	}
}

func TestMissingChannelWithoutEnsureFails(t *testing.T) {
	w := newWorld(t)
	res := w.run(func(o *Options) { o.EnsureChannel = false })
	if res.ExitCode() == 0 || w.hub.count("channel") != 0 {
		t.Fatalf("exit %d, %d channels created; want a failure and none", res.ExitCode(), w.hub.count("channel"))
	}
	if !strings.Contains(w.out.String(), "run once with -ensure-channel") {
		t.Errorf("the failure should say how to create it:\n%s", w.out.String())
	}
}

func TestSquattedChannelPostsNothing(t *testing.T) {
	w := newWorld(t)
	// post: anyone, so the hub itself would take the bot's posts: only the
	// recommender's own owner check stops them.
	w.hub.addChannel(w.other, DefaultSlug, "Not the bot", "", nil, w.ms(-day), "anyone")
	w.hub.addChannel(w.other, "orchids", "Orchids", "", nil, w.ms(-time.Hour), "members")
	res := w.run(nil)
	if res.ExitCode() == 0 || w.hub.count("msg") != 0 {
		t.Fatalf("exit %d, %d posts; want a failure and no posts", res.ExitCode(), w.hub.count("msg"))
	}
	if !strings.Contains(w.out.String(), "not this key") {
		t.Errorf("should say who owns it:\n%s", w.out.String())
	}
}

// New: created within the window, newest first. Active: posted in the last
// week, most active first. Old and quiet: neither.
func TestNewVersusActive(t *testing.T) {
	w := newWorld(t)
	w.hub.addChannel(w.other, "lichen-survey", "Lichen survey", "Field notes.", []string{"botany"}, w.ms(-2*day), "members")
	w.hub.addChannel(w.other, "tide-tables", "Tide tables", "", []string{"ocean"}, w.ms(-1*day), "members")
	busy := w.hub.addChannel(w.other, "loom-weaving", "Loom weaving", "", []string{"craft"}, w.ms(-40*day), "members")
	busier := w.hub.addChannel(w.other, "bee-keeping", "Bee keeping", "", []string{"apiary"}, w.ms(-50*day), "members")
	w.hub.addChannel(w.other, "old-quiet", "Old and quiet", "", nil, w.ms(-60*day), "members")
	w.hub.setActivity(busy, 3)
	w.hub.setActivity(busier, 9)
	if res := w.run(nil); res.ExitCode() != 0 {
		t.Fatalf("run failed:\n%s", w.out.String())
	}
	got := slugs(w.hub.recs(DefaultSlug))
	if want := "tide-tables/new lichen-survey/new bee-keeping/active loom-weaving/active"; got != want {
		t.Fatalf("recommended %q, want %q\n%s", got, want, w.out.String())
	}
	r := w.hub.recs(DefaultSlug)[0]
	if r.Title != "Tide tables" || strings.Join(r.Tags, ",") != "ocean" {
		t.Errorf("rec carries title %q tags %v", r.Title, r.Tags)
	}
}

// A channel whose current manifest is an update: "new" is decided by its
// genesis ts, fetched from the hub, not the update's.
func TestNewUsesGenesisTimeNotUpdateTime(t *testing.T) {
	w := newWorld(t)
	old := w.hub.addChannel(w.other, "renamed-old", "Renamed", "", nil, w.ms(-30*day), "members")
	upd := clankmail.Sign(w.other, encodeUpdate(w.other, old, "renamed-old", "Renamed again", w.ms(-time.Hour)))
	if code, _ := w.hub.store(upd); code != "" {
		t.Fatal(code)
	}
	w.hub.addChannel(w.other, "fresh-start", "Fresh", "", nil, w.ms(-3*day), "members")
	w.run(nil)
	if got := slugs(w.hub.recs(DefaultSlug)); got != "fresh-start/new" {
		t.Fatalf("recommended %q, want only fresh-start/new (the updated channel is 30 days old)", got)
	}
}

func TestCapPerRun(t *testing.T) {
	w := newWorld(t)
	for i, s := range []string{"aa-one", "bb-two", "cc-three", "dd-four", "ee-five", "ff-six", "gg-seven"} {
		w.hub.addChannel(w.other, s, "T "+s, "", nil, w.ms(-time.Duration(10+i)*time.Hour), "members")
	}
	res := w.run(func(o *Options) { o.MaxPerRun = 3 })
	if res.ExitCode() != 0 {
		t.Fatalf("run failed:\n%s", w.out.String())
	}
	if got := slugs(w.hub.recs(DefaultSlug)); got != "aa-one/new bb-two/new cc-three/new" {
		t.Fatalf("first run recommended %q, want the 3 newest", got)
	}
	w.now = w.now.Add(time.Hour)
	w.run(func(o *Options) { o.MaxPerRun = 3 })
	if got := slugs(w.hub.recs(DefaultSlug)[3:]); got != "dd-four/new ee-five/new ff-six/new" {
		t.Fatalf("second run recommended %q, want the next 3", got)
	}
}

func TestCooldown(t *testing.T) {
	w := newWorld(t)
	act := w.hub.addChannel(w.other, "chess-openings", "Chess openings", "", nil, w.ms(-90*day), "members")
	w.hub.setActivity(act, 4)
	w.run(nil)
	if got := slugs(w.hub.recs(DefaultSlug)); got != "chess-openings/active" {
		t.Fatalf("first run: %q", got)
	}
	// Within the cooldown: nothing, with or without the state file.
	w.now = w.now.Add(DefaultCooldown - time.Hour)
	w.run(nil)
	if err := os.Remove(w.statePath()); err != nil {
		t.Fatal(err)
	}
	w.run(nil)
	if n := len(w.hub.recs(DefaultSlug)); n != 1 {
		t.Fatalf("%d recs within the cooldown (state kept, then rebuilt from the log), want 1\n%s", n, w.out.String())
	}
	if !strings.Contains(w.out.String(), "rebuilt the cooldown") {
		t.Errorf("the run without state should rebuild from the log:\n%s", w.out.String())
	}
	// Past it: recommended again.
	w.now = w.now.Add(2 * time.Hour)
	w.run(nil)
	if got := slugs(w.hub.recs(DefaultSlug)); got != "chess-openings/active chess-openings/active" {
		t.Fatalf("after the cooldown: %q", got)
	}
}

// #recommendations is itself new and (once posted to) active: never recommended.
func TestNeverRecommendsItself(t *testing.T) {
	w := newWorld(t)
	pot := w.hub.addChannel(w.other, "pottery", "Pottery", "", nil, w.ms(-5*day), "members")
	w.hub.setActivity(pot, 2)
	w.run(nil)
	w.now = w.now.Add(DefaultCooldown + time.Hour)
	w.run(nil)
	for _, r := range w.hub.recs(DefaultSlug) {
		if r.Slug == DefaultSlug {
			t.Fatalf("the recommendations channel recommended itself:\n%s", w.out.String())
		}
	}
	if n := len(w.hub.recs(DefaultSlug)); n < 2 {
		t.Fatalf("only %d recs: the control (pottery recommended twice) did not run", n)
	}
}

func TestSmokeChannelsSkipped(t *testing.T) {
	w := newWorld(t)
	w.hub.addChannel(w.other, "smoke-12345", "Smoke", "", []string{"smoke"}, w.ms(-time.Hour), "members")
	w.hub.addChannel(w.other, "real-thing", "Real", "", []string{"pens"}, w.ms(-2*time.Hour), "members")
	w.run(nil)
	if got := slugs(w.hub.recs(DefaultSlug)); got != "real-thing/new" {
		t.Fatalf("recommended %q", got)
	}
}

func TestRateLimitStopsTheRun(t *testing.T) {
	w := newWorld(t)
	for _, s := range []string{"kayaks", "canoes", "rafts"} {
		w.hub.addChannel(w.other, s, s, "", nil, w.ms(-time.Hour), "members")
	}
	w.hub.rateAfterMsgs = 1
	res := w.run(nil)
	if res.ExitCode() == 0 || !res.RateLimited || len(w.hub.recs(DefaultSlug)) != 1 {
		t.Fatalf("exit %d rate_limited=%v recs=%d; want a failure after one post", res.ExitCode(), res.RateLimited, len(w.hub.recs(DefaultSlug)))
	}
	// The one that was posted is in the state: the next run skips it.
	w.hub.rateAfterMsgs = 0
	w.run(nil)
	recs := w.hub.recs(DefaultSlug)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Slug] = true
	}
	if len(recs) != 3 || len(seen) != 3 {
		t.Fatalf("after the limit lifts: %q, want each of the three once", slugs(recs))
	}
}

func TestDryRunSubmitsNothing(t *testing.T) {
	w := newWorld(t)
	w.hub.addChannel(w.other, "dry-goods", "Dry goods", "", nil, w.ms(-time.Hour), "members")
	res := w.run(func(o *Options) { o.DryRun = true })
	if res.ExitCode() != 0 || len(w.hub.submits) != 1 { // only addChannel's
		t.Fatalf("dry run: exit %d, submits %v", res.ExitCode(), w.hub.submits)
	}
	if len(res.Posted) != 1 || !strings.Contains(w.out.String(), "  | channel: #dry-goods") {
		t.Fatalf("dry run should print the rec:\n%s", w.out.String())
	}
	if _, err := os.Stat(w.statePath()); !os.IsNotExist(err) {
		t.Fatal("dry run wrote state")
	}
}

func keyID(k ed25519.PrivateKey) string { return clankmail.KeyID(k.Public().(ed25519.PublicKey)) }
