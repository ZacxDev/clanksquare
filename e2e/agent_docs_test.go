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
// appear in site/agents.md, or, for the paste prompt, in the landing page's
// copy box, so the two front doors cannot drift apart.
func TestReadmeTryItMatchesAgentsMD(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join("..", p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	readme, agents := read("README.md"), read("site/agents.md")
	page := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(read("site/index.html"), "")
	parts := strings.SplitN(readme, "\n## Try it\n", 2)
	if len(parts) != 2 {
		t.Fatal(`README has no "## Try it" section`)
	}
	sec := strings.SplitN(parts[1], "\n## ", 2)[0]
	comment := regexp.MustCompile(`\s+#\s.*$`)
	n := 0
	for _, m := range regexp.MustCompile("(?s)```[a-z]*\n(.*?)```").FindAllStringSubmatch(sec, -1) {
		for _, l := range strings.Split(strings.TrimSpace(m[1]), "\n") {
			c := strings.TrimSpace(comment.ReplaceAllString(l, ""))
			if c == "" {
				continue
			}
			n++
			if !strings.Contains(agents, c) && !strings.Contains(page, c) {
				t.Errorf("README Try it line not in site/agents.md or the landing page: %q", c)
			}
		}
	}
	for _, m := range regexp.MustCompile("`(\\{\"mcpServers\".*?)`").FindAllStringSubmatch(sec, -1) {
		n++
		if !strings.Contains(agents, m[1]) {
			t.Errorf("README mcpServers snippet not in site/agents.md: %q", m[1])
		}
	}
	if n < 9 {
		t.Fatalf("checked only %d README Try it lines; the section parse is broken", n)
	}
}
