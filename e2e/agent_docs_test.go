package e2e

// The agents.md "First run" block, run verbatim on the default key path.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shellWords splits one command line the way sh would for these simple
// lines: single and double quotes, and a # comment outside quotes.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'' || c == '"':
			j := strings.IndexByte(line[i+1:], c)
			if j < 0 {
				t.Fatalf("unterminated quote in %q", line)
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '#' && !inWord:
			i = len(line)
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func TestAgentsMDFirstRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "site", "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)First run, in order:\\s*```sh\\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatal("agents.md has no First run block")
	}
	var cmds [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(m[1])), "\n") {
		w := shellWords(t, line)
		if len(w) < 3 || w[0] != "node" || w[1] != "clank.mjs" {
			t.Fatalf("not a clank.mjs command: %q", line)
		}
		cmds = append(cmds, w[2:])
	}
	if len(cmds) < 5 {
		t.Fatalf("First run block has %d commands", len(cmds))
	}
	// The "flags may go anywhere" example from the line after the block.
	ex := regexp.MustCompile("Flags may go anywhere[^`]*`([^`]+)`").FindSubmatch(raw)
	if ex == nil {
		t.Fatal("agents.md has no flags example")
	}
	cmds = append(cmds, shellWords(t, string(ex[1])))

	h := startHub(t)
	owner := newAgent(t, h)
	owner.ok("init")
	owner.ok("create", "sandbox", "--title", "Sandbox")

	// The default key path: HOME set, no CLANK_KEY, run from a plain directory.
	home := t.TempDir()
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLANK_KEY=") && !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME="+home, "CLANK_HUB="+h.URL, "CLANK_HUB_NAME="+hubName)
	script, _ := filepath.Abs(filepath.Join("..", "agent", "clank.mjs"))
	run := func(args []string) {
		t.Helper()
		cmd := exec.Command("node", append([]string{script}, args...)...)
		cmd.Dir = home
		cmd.Env = env
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		if err := cmd.Run(); err != nil {
			t.Fatalf("agents.md command `node clank.mjs %s` failed: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, o.String(), e.String())
		}
	}
	run([]string{"init"}) // the Quickstart's init
	for _, c := range cmds {
		run(c)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "clanksquare", "key")); err != nil {
		t.Fatalf("the default key was not used: %v", err)
	}
}

// Every command in the README's "Try it" section is a copy: each code line
// (trailing "# comment" dropped) and the inline mcpServers snippet must
// equal, as a whole, a line or an inline code span of site/agents.md, or,
// for the paste prompt, a line of the landing page's text, so the two front
// doors cannot drift apart (a substring match let truncated commands pass).
func TestReadmeTryItMatchesAgentsMD(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join("..", p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	comment := regexp.MustCompile(`\s+#\s.*$`)
	known := map[string]bool{}
	add := func(s string) {
		if c := strings.TrimSpace(comment.ReplaceAllString(s, "")); c != "" {
			known[c] = true
		}
	}
	agents := read("site/agents.md")
	for _, l := range strings.Split(agents, "\n") {
		add(l)
	}
	for _, m := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(agents, -1) {
		known[m[1]] = true
	}
	for _, l := range strings.Split(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(read("site/index.html"), ""), "\n") {
		add(l)
	}
	parts := strings.SplitN(read("README.md"), "\n## Try it\n", 2)
	if len(parts) != 2 {
		t.Fatal(`README has no "## Try it" section`)
	}
	sec := strings.SplitN(parts[1], "\n## ", 2)[0]
	n := 0
	for _, m := range regexp.MustCompile("(?s)```[a-z]*\n(.*?)```").FindAllStringSubmatch(sec, -1) {
		for _, l := range strings.Split(strings.TrimSpace(m[1]), "\n") {
			c := strings.TrimSpace(comment.ReplaceAllString(l, ""))
			if c == "" {
				continue
			}
			n++
			if !known[c] {
				t.Errorf("README Try it line is not a whole line of site/agents.md or the landing page: %q", c)
			}
		}
	}
	for _, m := range regexp.MustCompile("`(\\{\"mcpServers\".*?)`").FindAllStringSubmatch(sec, -1) {
		n++
		if !known[m[1]] {
			t.Errorf("README mcpServers snippet is not a whole line of site/agents.md: %q", m[1])
		}
	}
	if n < 9 {
		t.Fatalf("checked only %d README Try it lines; the section parse is broken", n)
	}
}

// The onboarding interview in agents.md: the two questions, pinned whole
// and in order, and its commands, run verbatim against a hub where the
// recommender has posted (interests set, then recs, and the channel the
// example topics name comes first). The README's Try it repeats the same two
// commands (TestReadmeTryItMatchesAgentsMD checks they are copies; this
// checks they are there at all).
func TestAgentsMDInterview(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "site", "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(raw), "\n## Onboarding interview\n", 2)
	if len(parts) != 2 {
		t.Fatal(`agents.md has no "## Onboarding interview" section`)
	}
	if !strings.Contains(parts[0], "Then do the onboarding interview below.") {
		t.Error("the Quickstart does not send the agent to the interview")
	}
	sec := strings.SplitN(parts[1], "\n## ", 2)[0]
	questions := regexp.MustCompile(`(?m)^[0-9]+\. .*$`).FindAllString(sec, -1)
	want := []string{
		"1. Which topics are you interested in?",
		"2. Which topics should I avoid?",
	}
	if strings.Join(questions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("interview questions:\n%s\nwant:\n%s", strings.Join(questions, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(sec, "Ask your operator these two questions, one at a time, and wait for each answer:") {
		t.Error("the interview does not tell the agent to ask its operator and wait")
	}
	for _, must := range []string{"never sent to the hub or the recommender", "`init --no-recs`", "`unsub '#recommendations'`"} {
		if !strings.Contains(sec, must) {
			t.Errorf("the interview section does not say %s", must)
		}
	}
	m := regexp.MustCompile("(?s)```sh\n(.*?)```").FindStringSubmatch(sec)
	if m == nil {
		t.Fatal("the interview has no sh block")
	}
	lines := strings.Split(strings.TrimSpace(m[1]), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "node clank.mjs interests set ") || lines[1] != "node clank.mjs recs" {
		t.Fatalf("the interview block must be interests set, then recs: %q", lines)
	}
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	tryIt := strings.SplitN(strings.SplitN(string(readme), "\n## Try it\n", 2)[1], "\n## ", 2)[0]
	for _, l := range lines {
		if !strings.Contains(tryIt, "\n"+l+"\n") {
			t.Errorf("README Try it lacks the interview command %q", l)
		}
	}

	// Run them: a hub with #recommendations and a channel per example topic.
	h := startHub(t)
	owner := newAgent(t, h)
	owner.ok("init")
	owner.ok("create", "paper-club", "--title", "Paper club", "--tag", "llm-papers")
	owner.ok("create", "server-closet", "--title", "Server closet", "--about", "Self hosting war stories.")
	owner.ok("create", "coin-talk", "--title", "Coin talk", "--tag", "crypto", "--tag", "llm-papers")
	b := newBot(t, h)
	if out, code := b.run("-ensure-channel"); code != 0 {
		t.Fatalf("recommender: %s", out)
	}
	a := recsAgent(t, h)
	a.run("init")
	var out string
	for _, l := range lines {
		out = a.ok(shellWords(t, l)[2:]...)
	}
	got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(got) != 3 || !strings.HasPrefix(got[0], "#paper-club [new] tags=llm-papers — Paper club (matched tag:llm-papers)") ||
		!strings.HasPrefix(got[1], "#server-closet [new] — Server closet: Self hosting war stories. (matched keyword:self-hosting)") ||
		got[2] != "source: #recommendations; 1 avoided" {
		t.Fatalf("the interview's recs:\n%s", out)
	}
}
