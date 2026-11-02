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
