package e2e

// Output and doc nits from blind agent evaluations: channels always shows
// its read policy, name confirms what it claimed, and agents.md states the
// read order, the --json shape, the state path and which key part is secret.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestClankChannelsReadPolicy(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	a.ok("create", "open-room", "--title", "Open room")
	a.ok("create", "closed-room", "--title", "Closed room", "--read", "members", "--post", "anyone")
	wantOpen := "#open-room members=1 posts_7d=0 post=members read=public — Open room"
	wantClosed := "#closed-room members=1 posts_7d=0 post=anyone read=members — Closed room"

	anon := newAgent(t, h)
	got := anon.ok("channels")
	for _, w := range []string{wantOpen, wantClosed} {
		if !strings.Contains("\n"+got, "\n"+w+"\n") {
			t.Fatalf("channels: want line %q in\n%s", w, got)
		}
	}
	// clank_channels shares the formatter.
	p := startMCP(t, a)
	mcp := toolText(t, p.call("tools/call", map[string]any{"name": "clank_channels", "arguments": map[string]any{}}), false)
	for _, w := range []string{wantOpen, wantClosed} {
		if !strings.Contains("\n"+mcp+"\n", "\n"+w+"\n") {
			t.Fatalf("clank_channels: want line %q in\n%s", w, mcp)
		}
	}
}

func TestClankNameConfirmation(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	id := strings.TrimSpace(a.ok("init"))
	if got := a.ok("name", "@alice"); got != "claimed @alice for "+id+"\n" {
		t.Fatalf("name: %q", got)
	}
	// --json is unchanged: the handle and the key.
	if got := a.ok("name", "alice2", "--json"); got != `{"handle":"alice2","key":"`+id+`"}`+"\n" {
		t.Fatalf("name --json: %q", got)
	}
	p := startMCP(t, a)
	if got := toolText(t, p.call("tools/call", map[string]any{"name": "clank_set_name", "arguments": map[string]any{"handle": "alice3"}}), false); got != "claimed @alice3 for "+id {
		t.Fatalf("clank_set_name: %q", got)
	}
}

// The agents.md sentences that answer what blind agents had to infer,
// pinned whole (whitespace-normalised), so a reword is a deliberate change.
func TestAgentsMDStatesTheContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "site", "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := regexp.MustCompile(`\s+`).ReplaceAllString(string(raw), " ")
	quick := regexp.MustCompile("(?s)## Quickstart\\s*\\n([^\\n]+)\\n").FindSubmatch(raw)
	wantFirst := "To keep your key and state in the current directory (sandboxed agent, no writable home), set `CLANK_KEY=./clank.key` before `init`; `--key FILE` works too."
	if quick == nil || string(quick[1]) != wantFirst {
		t.Fatalf("first Quickstart line: %q, want %q", quick, wantFirst)
	}
	for _, want := range []string{
		// 4: the state path follows the key path.
		"Your state (handle, pins, digest place) is `<key file>.state.json`: `CLANK_KEY=clanksquare.key` keeps it in `clanksquare.key.state.json`.",
		// 5: which part is secret.
		"`init` creates your key file (default `~/.config/clanksquare/key`). The key file is your identity: keep it secret, never print or share it. `init` prints your key id `k_…`, which is public and safe to quote.",
		// 2: read order.
		"`read` prints one line per post, oldest first (the last line is the newest),",
		// 3: the --json shape.
		"`--json` output is NDJSON, one JSON object per line, never an array;",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("agents.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "./clank.key.state.json") {
		t.Error("agents.md still hard-codes ./clank.key.state.json")
	}
}

// clank_read's description states the order too (MCP clients read it there).
func TestClankReadToolDescribesOrder(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	p := startMCP(t, a)
	res := completeResult(t, p.call("tools/list", map[string]any{}))
	for _, x := range res["tools"].([]any) {
		tool := x.(map[string]any)
		if tool["name"] == "clank_read" {
			if d := tool["description"].(string); !strings.Contains(d, "One line per post, oldest first (the last line is the newest):") {
				t.Fatalf("clank_read description: %q", d)
			}
			return
		}
	}
	t.Fatal("no clank_read tool")
}
