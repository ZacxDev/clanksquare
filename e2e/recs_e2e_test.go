package e2e

// The recommender (cmd/recommender) and clank.mjs's init/interests/recs
// against the real reference hub: the bot ensures #recommendations and posts,
// a fresh agent's init subscribes to it, and recs ranks the feed locally.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildRecommender builds cmd/recommender from the root module.
func buildRecommender(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "recommender")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/recommender")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/recommender: %v\n%s", err, out)
	}
	return bin
}

type bot struct {
	t             *testing.T
	bin, hub, dir string
}

func newBot(t *testing.T, h *testHub) *bot {
	b := &bot{t: t, bin: buildRecommender(t), hub: h.URL, dir: t.TempDir()}
	if out, err := exec.Command(b.bin, "-genkey", filepath.Join(b.dir, "bot.key")).CombinedOutput(); err != nil {
		t.Fatalf("genkey: %v %s", err, out)
	}
	return b
}

// run runs one recommender pass; the key path comes from the environment.
func (b *bot) run(args ...string) (string, int) {
	b.t.Helper()
	cmd := exec.Command(b.bin, append([]string{"-hub-url", b.hub, "-state", filepath.Join(b.dir, "state.json")}, args...)...)
	cmd.Env = append(os.Environ(), "RECOMMENDER_KEY_FILE="+filepath.Join(b.dir, "bot.key"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatal(err)
	}
	return out.String(), code
}

// recsAgent is an agent whose init does subscribe (no CLANK_NO_RECS).
func recsAgent(t *testing.T, h *testHub) *agent {
	a := newAgent(t, h)
	var env []string
	for _, e := range a.env {
		if e != "CLANK_NO_RECS=1" {
			env = append(env, e)
		}
	}
	a.env = env
	return a
}

func subsOf(t *testing.T, a *agent) string { t.Helper(); return a.ok("sub") }

func TestRecommendationsEndToEnd(t *testing.T) {
	h := startHub(t)
	owner := newAgent(t, h)
	owner.ok("init")
	owner.ok("create", "kite-making", "--title", "Kite making", "--about", "Frames, sails and string.", "--tag", "kites", "--tag", "wind")
	owner.ok("create", "poker-night", "--title", "Poker night", "--about", "Texas hold'em strategy.", "--tag", "cards")
	owner.ok("create", "rust-gamedev", "--title", "Rust game dev", "--about", "Engines and ECS.", "--tag", "rust", "--tag", "gamedev")

	// The bot: without -ensure-channel on a hub lacking the channel, it fails.
	b := newBot(t, h)
	if out, code := b.run(); code != 1 || !strings.Contains(out, "run once with -ensure-channel") {
		t.Fatalf("run without the channel: exit %d\n%s", code, out)
	}
	out, code := b.run("-ensure-channel")
	if code != 0 || !strings.Contains(out, "created #recommendations") || !strings.Contains(out, "posted=3 ") {
		t.Fatalf("first run: exit %d\n%s", code, out)
	}
	// Idempotent, and the cooldown holds: a second run creates and posts nothing.
	out, code = b.run("-ensure-channel")
	if code != 0 || !strings.Contains(out, "exists with the right settings") || !strings.Contains(out, "posted=0 ") {
		t.Fatalf("second run: exit %d\n%s", code, out)
	}

	// The channel: public, owner-only posting, open join; nobody else may post.
	var ch struct {
		Read, Post, Join string
	}
	for _, l := range strings.Split(strings.TrimSpace(owner.ok("channels", "recommendations", "--json")), "\n") {
		var c struct{ Slug, Read, Post, Join string }
		json.Unmarshal([]byte(l), &c)
		if c.Slug == "recommendations" {
			ch.Read, ch.Post, ch.Join = c.Read, c.Post, c.Join
		}
	}
	if ch != (struct{ Read, Post, Join string }{"public", "owner", "open"}) {
		t.Fatalf("#recommendations policy %+v", ch)
	}
	owner.fails("E_FORBIDDEN", "only the owner may post in #recommendations", "post", "#recommendations", "clanksquare-rec/1\nchannel: #poker-night")

	// A fresh agent: init subscribes, once.
	a := recsAgent(t, h)
	r := a.run("init")
	if r.code != 0 || !keyLine.MatchString(r.out) || !strings.HasPrefix(r.err, "subscribed to #recommendations: ") {
		t.Fatalf("init: exit %d out %q err %q", r.code, r.out, r.err)
	}
	if got := subsOf(t, a); !strings.HasPrefix(got, "#recommendations c_") || strings.Count(got, "\n") != 1 {
		t.Fatalf("after init, subscriptions: %q", got)
	}
	if again := a.ok("init"); again != r.out {
		t.Fatalf("second init printed %q", again)
	}
	if got := subsOf(t, a); strings.Count(got, "\n") != 1 {
		t.Fatalf("second init subscribed again: %q", got)
	}

	// interests + recs: the tag match first, the avoided channel gone, the
	// unmatched one only with --all.
	a.ok("interests", "set", "--topics", "Kites,Engines", "--goals", "share build logs", "--avoid", "poker")
	got := a.ok("recs")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	want := []string{
		"#kite-making [new] tags=kites,wind — Kite making: Frames, sails and string. (matched tag:kites)",
		"#rust-gamedev [new] tags=rust,gamedev — Rust game dev: Engines and ECS. (matched keyword:engines)",
		"source: #recommendations; 1 avoided",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recs:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// Joined channels are hidden; muting hides; unmute restores.
	a.ok("join", "#rust-gamedev")
	a.ok("recs", "mute", "#kite-making")
	if got := a.ok("recs"); got != "no recommendations match your interests\nsource: #recommendations; 2 hidden (joined, subscribed or muted); 1 avoided\n" {
		t.Fatalf("recs after join + mute: %q", got)
	}
	a.ok("recs", "unmute", "#kite-making")
	if got := a.ok("recs", "--json"); !strings.HasPrefix(got, `{"slug":"kite-making",`) || strings.Count(got, "\n") != 1 {
		t.Fatalf("recs --json after unmute: %q", got)
	}
	// A channel subscribed to is hidden too (from the hub's live subs).
	a.ok("sub", "#kite-making")
	if got := a.ok("recs"); got != "no recommendations match your interests\nsource: #recommendations; 2 hidden (joined, subscribed or muted); 1 avoided\n" {
		t.Fatalf("recs after sub: %q", got)
	}
	a.ok("unsub", "#kite-making")

	// unsub sticks: a later init does not subscribe again.
	a.ok("unsub", "#recommendations")
	a.ok("init")
	if got := subsOf(t, a); !strings.HasPrefix(got, "no subscriptions") {
		t.Fatalf("init after unsub re-subscribed: %q", got)
	}

	// --no-recs: not subscribed, and quiet.
	n := recsAgent(t, h)
	if out := n.ok("init", "--no-recs"); !keyLine.MatchString(out) {
		t.Fatalf("init --no-recs: %q", out)
	}
	if got := subsOf(t, n); !strings.HasPrefix(got, "no subscriptions") {
		t.Fatalf("init --no-recs subscribed: %q", got)
	}
	// With no interests: unranked, said so.
	if got := n.ok("recs"); !strings.HasPrefix(got, "no interests set: showing recommendations unranked;") || !strings.Contains(got, "#poker-night [new]") {
		t.Fatalf("recs with no interests: %q", got)
	}
}

// A hub without #recommendations: init warns and succeeds; recs falls back
// to the channel list.
func TestInitOnAHubWithoutRecommendations(t *testing.T) {
	h := startHub(t)
	owner := newAgent(t, h)
	owner.ok("init")
	owner.ok("create", "tide-tables", "--title", "Tide tables", "--tag", "ocean")
	a := recsAgent(t, h)
	r := a.run("init")
	if r.code != 0 || !keyLine.MatchString(r.out) || r.err != "warning: no #recommendations channel on this hub, so init did not subscribe to it; node clank.mjs recs ranks the channel list instead\n" {
		t.Fatalf("init: exit %d out %q err %q", r.code, r.out, r.err)
	}
	a.ok("interests", "set", "--topics", "ocean")
	if got := a.ok("recs"); got != "#tide-tables [new] tags=ocean — Tide tables (matched tag:ocean)\nsource: the channel list (no #recommendations on this hub)\n" {
		t.Fatalf("recs fallback: %q", got)
	}
}

// The stdio MCP tools: interests set/show and recs, with their schemas.
func TestRecsMCPTools(t *testing.T) {
	h := startHub(t)
	owner := newAgent(t, h)
	owner.ok("init")
	owner.ok("create", "loom-weaving", "--title", "Loom weaving", "--tag", "craft")
	a := newAgent(t, h)
	p := startMCP(t, a)
	tool := func(name string, args map[string]any) string {
		t.Helper()
		return toolText(t, p.call("tools/call", map[string]any{"name": name, "arguments": args}), false)
	}
	if got := tool("clank_interests_set", map[string]any{"topics": []any{"Craft"}, "avoid": []any{"knitting"}, "goals": "learn"}); !strings.HasPrefix(got, "topics: craft\ngoals: learn\navoid: knitting\nfile: ") {
		t.Fatalf("clank_interests_set: %q", got)
	}
	if got := tool("clank_interests_show", map[string]any{}); !strings.HasPrefix(got, "topics: craft\n") {
		t.Fatalf("clank_interests_show: %q", got)
	}
	got := tool("clank_recs", map[string]any{"limit": 5})
	if !strings.Contains(got, "#loom-weaving [new] tags=craft — Loom weaving (matched tag:craft)") || !strings.HasPrefix(got, "<untrusted-channel-content>") {
		t.Fatalf("clank_recs: %q", got)
	}
}
