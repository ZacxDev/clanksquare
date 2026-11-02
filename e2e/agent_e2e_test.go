package e2e

// End-to-end tests for agent/clank.mjs (the CLI, the stdio MCP server and the
// hosted read-only MCP server) against the real reference hub. They run the
// committed, generated agent/clank.mjs with node, exactly as an agent would.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clankmail/go/hub"
)

var clankScript = filepath.Join("..", "agent", "clank.mjs")

// testHub runs the reference hub. extra, when set, returns log items to
// append to every channel-log response (the tampering tests use it).
type testHub struct {
	URL   string
	extra atomic.Pointer[func(channel string) []map[string]string]
}

func startHub(t *testing.T) *testHub {
	t.Helper()
	store, err := hub.OpenSQLite(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := hub.New(hub.Config{Name: hubName, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	th := &testHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := th.extra.Load()
		if f == nil || !strings.HasSuffix(r.URL.Path, "/log") {
			h.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var page struct {
			Items []map[string]string `json:"items"`
			Next  string              `json:"next"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		// Only on the first page, so a reader that pages on sees them once.
		if r.URL.Query().Get("after") == "" {
			page.Items = append(page.Items, (*f)(strings.Split(r.URL.Path, "/")[3])...)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	th.URL = srv.URL
	return th
}

type agent struct {
	t   *testing.T
	env []string
	key string
}

func newAgent(t *testing.T, h *testHub) *agent {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node is required for the clank.mjs tests: ", err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "cfg", "clanksquare", "key")
	return &agent{t: t, key: key, env: append(os.Environ(),
		"CLANK_HUB="+h.URL, "CLANK_HUB_NAME="+hubName, "CLANK_KEY="+key, "HOME="+dir)}
}

type result struct {
	out, err string
	code     int
}

func (a *agent) runIn(stdin string, args ...string) result {
	a.t.Helper()
	// A command that should exit but serves instead must fail, not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", append([]string{clankScript}, args...)...)
	cmd.Env = a.env
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		a.t.Fatal(err)
	}
	return result{o.String(), e.String(), code}
}

func (a *agent) run(args ...string) result { a.t.Helper(); return a.runIn("", args...) }

// ok runs a command that must succeed and print nothing to stderr.
func (a *agent) ok(args ...string) string {
	a.t.Helper()
	r := a.run(args...)
	if r.code != 0 || r.err != "" {
		a.t.Fatalf("clank %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.out, r.err)
	}
	return r.out
}

// fails runs a command that must fail with exactly one stderr line
// "error: <code>: ...", containing want.
func (a *agent) fails(code, want string, args ...string) {
	a.t.Helper()
	r := a.run(args...)
	line := strings.TrimSuffix(r.err, "\n")
	if r.code == 0 || strings.Contains(line, "\n") || !strings.HasPrefix(line, "error: "+code+": ") || !strings.Contains(line, want) || r.out != "" {
		a.t.Fatalf("clank %s: want exit != 0 and one line 'error: %s: ...%s...'\ngot exit %d\nstdout: %q\nstderr: %q", strings.Join(args, " "), code, want, r.code, r.out, r.err)
	}
}

var (
	keyLine  = regexp.MustCompile(`^k_[a-z2-7]{52}\n$`)
	postLine = regexp.MustCompile(`^m_[a-z2-7]{8} (#[a-z0-9-]+ )?(@[a-z0-9-]+|k_[a-z2-7]{4}…[a-z2-7]{4}) \d{4}-\d\d-\d\dT\d\d:\d\dZ (re:m_[a-z2-7]{8} )?\S`)
	nextLine = regexp.MustCompile(`^next: --after [0-9]+`)
)

// postLines splits read/digest output into post lines and the cursor line.
func postLines(t *testing.T, out string) ([]string, string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !nextLine.MatchString(last) {
		t.Fatalf("output does not end with a next: line:\n%s", out)
	}
	var posts []string
	for _, l := range lines[:len(lines)-1] {
		if l == "no posts" || l == "no new posts" {
			continue
		}
		if !postLine.MatchString(l) {
			t.Fatalf("not a post line: %q", l)
		}
		posts = append(posts, l)
	}
	return posts, last
}

func bodies(lines []string) []string {
	var b []string
	for _, l := range lines {
		f := strings.SplitN(l, " ", 4)
		rest := f[3]
		if strings.HasPrefix(rest, "re:") {
			rest = strings.SplitN(rest, " ", 2)[1]
		}
		b = append(b, rest)
	}
	return b
}

func TestClankCLI(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	b := newAgent(t, h)

	// init: creates the key once, prints only its id, never the seed.
	idA := a.ok("init")
	if !keyLine.MatchString(idA) {
		t.Fatalf("init printed %q, want one k_ id", idA)
	}
	if again := a.ok("init"); again != idA {
		t.Fatalf("init is not idempotent: %q then %q", idA, again)
	}
	seed, _ := os.ReadFile(a.key)
	if strings.Contains(idA, strings.TrimSpace(string(seed))) {
		t.Fatal("init printed the seed")
	}
	if st, _ := os.Stat(a.key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(a.key)); st.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %v, want 0700", st.Mode().Perm())
	}
	idB := strings.TrimSpace(b.ok("init"))

	// name, whoami
	if got := a.ok("name", "alice"); got != "@alice\n" {
		t.Fatalf("name: %q", got)
	}
	if got := a.ok("whoami"); got != "key="+strings.TrimSpace(idA)+" handle=@alice hub="+h.URL+"\n" {
		t.Fatalf("whoami: %q", got)
	}
	b.fails("E_FORBIDDEN", "@alice is taken", "name", "alice")

	// create
	created := a.ok("create", "sandbox", "--title", "Sandbox", "--about", "Test posting here.", "--tag", "test")
	if !regexp.MustCompile(`^created #sandbox c_[a-z2-7]{52}\n$`).MatchString(created) {
		t.Fatalf("create: %q", created)
	}
	b.fails("E_FORBIDDEN", "#sandbox is taken", "create", "sandbox", "--title", "Mine")
	a.fails("E_QUERY", "bad slug", "create", "Bad_Slug", "--title", "x")

	// channels: no key needed, the new channel is listed.
	anon := newAgent(t, h)
	if got := anon.ok("channels"); !strings.HasPrefix(got, "#sandbox members=1 posts_7d=0 post=members — Sandbox: Test posting here.\n") {
		t.Fatalf("channels: %q", got)
	}
	if _, err := os.Stat(anon.key); !os.IsNotExist(err) {
		t.Fatal("channels created a key")
	}

	// Error paths: posting before joining, a bad slug, an unknown channel, no key.
	b.fails("E_FORBIDDEN", "you are not a member of #sandbox: node clank.mjs join #sandbox first", "post", "#sandbox", "hi")
	b.fails("E_QUERY", "bad channel name", "read", "#Bad_Slug")
	b.fails("E_NOT_FOUND", "no channel #nope on this hub: find one with node clank.mjs channels", "read", "#nope")
	anon.fails("E_NOKEY", "run node clank.mjs init first", "post", "#sandbox", "hi")
	a.fails("E_USAGE", "--limit must be", "read", "#sandbox", "--limit", "0")

	// B subscribes before anything is posted (a sub covers later posts only).
	if got := b.ok("sub", "#sandbox"); got != "subscribed #sandbox\n" {
		t.Fatalf("sub: %q", got)
	}
	b.fails("E_FORBIDDEN", "already subscribed to #sandbox", "sub", "#sandbox")

	// join + post; the owner is a member already, B joins.
	if got := b.ok("join", "#sandbox"); got != "joined #sandbox\n" {
		t.Fatalf("join: %q", got)
	}
	posted := a.ok("post", "#sandbox", "hello", "world\nsecond line")
	m := regexp.MustCompile(`^posted (m_[a-z2-7]{52}) #sandbox\n$`).FindStringSubmatch(posted)
	if m == nil {
		t.Fatalf("post: %q", posted)
	}
	helloID := m[1]

	// The second key reads what the first posted.
	posts, _ := postLines(t, b.ok("read", "#sandbox"))
	if len(posts) != 1 || !strings.HasPrefix(posts[0], helloID[:10]+" "+shortKey(idA)+" ") || !strings.HasSuffix(posts[0], " hello world") {
		t.Fatalf("B reads %q", posts)
	}
	// After `who`, the pinned handle is shown instead of the key.
	if got := b.ok("who", "@alice"); got != "@alice "+strings.TrimSpace(idA)+"\n" {
		t.Fatalf("who: %q", got)
	}
	posts, _ = postLines(t, b.ok("read", "#sandbox"))
	if !strings.Contains(posts[0], " @alice ") {
		t.Fatalf("after who, B reads %q", posts)
	}

	// Reply by short id, and a body from stdin.
	r := b.runIn("from stdin\n", "post", "#sandbox", "-", "--reply-to", helloID[:10])
	if r.code != 0 || !strings.HasPrefix(r.out, "posted m_") {
		t.Fatalf("post from stdin: %+v", r)
	}
	full := a.ok("read", "#sandbox", "--full")
	posts, next := postLines(t, a.ok("read", "#sandbox"))
	if len(posts) != 2 || !strings.HasSuffix(posts[1], " re:"+helloID[:10]+" from stdin") || !strings.Contains(posts[0], " @alice ") {
		t.Fatalf("A reads %q", posts)
	}
	if !strings.Contains(full, "\n  hello world\n  second line\n") {
		t.Fatalf("--full does not show the whole body:\n%s", full)
	}

	// --after: nothing new, then exactly the new post.
	cursor := strings.Fields(next)[2]
	if got := a.ok("read", "#sandbox", "--after", cursor); got != "no new posts\nnext: --after "+cursor+"\n" {
		t.Fatalf("read --after (nothing new): %q", got)
	}
	a.ok("post", "#sandbox", "third")
	posts, _ = postLines(t, a.ok("read", "#sandbox", "--after", cursor))
	if len(posts) != 1 || !strings.HasSuffix(posts[0], " third") {
		t.Fatalf("read --after: %q", posts)
	}

	// --json: one JSON object per post with the full fields.
	js := a.ok("read", "#sandbox", "--json", "--limit", "1")
	var obj map[string]any
	if strings.Count(js, "\n") != 1 || json.Unmarshal([]byte(js), &obj) != nil || obj["body"] != "third" || obj["from"] != strings.TrimSpace(idA) || obj["handle"] != "alice" || obj["cursor"] == nil {
		t.Fatalf("read --json: %q", js)
	}

	// digest: B sees A's posts (not its own), ranked; the cursor is saved.
	posts, _ = postLines(t, b.ok("digest"))
	if len(posts) != 2 || !strings.HasPrefix(posts[0], helloID[:10]+" #sandbox @alice ") || !strings.HasSuffix(posts[0], "Z [1 repliers/1 replies] hello world") || !strings.HasSuffix(posts[1], "Z third") {
		t.Fatalf("digest: %q", posts)
	}
	if got := b.ok("digest"); !strings.HasPrefix(got, "no new posts\nnext: --after ") {
		t.Fatalf("second digest: %q", got)
	}
	if got := b.ok("sub"); got != "#sandbox "+strings.Fields(created)[2]+"\n" {
		t.Fatalf("sub (list): %q", got)
	}
	if got := b.ok("unsub", "#sandbox"); got != "unsubscribed #sandbox\n" {
		t.Fatalf("unsub: %q", got)
	}
	b.fails("E_NOT_FOUND", "not subscribed to #sandbox", "unsub", "#sandbox")

	// A members-only channel: its log needs a signed, authenticated read.
	a.ok("create", "inner", "--title", "Inner", "--read", "members")
	a.ok("post", "#inner", "members only")
	b.fails("E_FORBIDDEN", "#inner is members-only: node clank.mjs join #inner first", "read", "#inner")
	anon.fails("E_NOKEY", "run node clank.mjs init first", "read", "#inner")
	b.ok("join", "#inner")
	posts, _ = postLines(t, b.ok("read", "#inner"))
	if len(posts) != 1 || !strings.HasSuffix(posts[0], "Z members only") {
		t.Fatalf("member reads #inner: %q", posts)
	}
	_ = idB
}

func shortKey(k string) string {
	k = strings.TrimSpace(k)
	return k[:6] + "…" + k[len(k)-4:]
}

// TestClankVerifiesAndCleans: the hub is not trusted. A forged token and a
// post addressed to another channel are dropped and counted; a validly signed
// post full of control, bidi and invisible characters is shown cleaned.
func TestClankVerifiesAndCleans(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	a.ok("create", "verify", "--title", "Verify")
	a.ok("post", "#verify", "genuine")
	var ch struct{ Channel string }
	getJSON(t, h.URL+"/v1/channels/%23verify", &ch)
	var page struct{ Items []map[string]string }
	getJSON(t, h.URL+"/v1/channels/"+ch.Channel+"/log", &page)
	genuine := page.Items[0]["token"]

	_, priv, _ := ed25519.GenerateKey(nil)
	from := clankmail.KeyID(priv.Public().(ed25519.PublicKey))
	signed := func(to, body string) string {
		p, _ := json.Marshal(map[string]any{"v": 1, "t": "msg", "from": from, "ts": time.Now().UnixMilli(), "to": []string{to}, "body": body})
		return clankmail.Sign(priv, p)
	}
	// A forged token: the genuine post's payload with someone else's signature.
	parts := strings.Split(genuine, ".")
	other := strings.Split(signed(ch.Channel, "x"), ".")
	forged := strings.Join([]string{parts[0], parts[1], parts[2], other[3]}, ".")
	hostile := "evil\u202egnp.exe\u200b \x1b[31mred\x1b[0m\u0007 \u2066x\u2069"
	elsewhere := "c_" + strings.Repeat("a", 52)
	extra := func(string) []map[string]string {
		return []map[string]string{
			{"cursor": "900001", "token": forged},
			{"cursor": "900002", "token": signed(elsewhere, "misaddressed")},
			{"cursor": "900003", "token": signed(ch.Channel, hostile)},
		}
	}
	h.extra.Store(&extra)

	r := a.run("read", "#verify")
	if r.code != 0 {
		t.Fatalf("read: %+v", r)
	}
	posts, _ := postLines(t, r.out)
	got := bodies(posts)
	if len(got) != 2 || got[0] != "genuine" || got[1] != "evilgnp.exe [31mred[0m x" {
		t.Fatalf("read shows %q", got)
	}
	if r.err != "warning: dropped 2 entries that failed signature verification (SPEC §4); they are not shown\n" {
		t.Fatalf("stderr: %q", r.err)
	}
	if strings.ContainsAny(r.out, "\u202e\u200b\x1b\u0007\u2066\u2069") || strings.Contains(r.out, "misaddressed") {
		t.Fatalf("read printed a control/bidi character or a dropped post: %q", r.out)
	}
	js := a.run("read", "#verify", "--json")
	if strings.ContainsAny(js.out, "\u202e\u200b\u2066") || strings.Contains(js.out, `\u001b`) || strings.Count(js.out, "\n") != 2 {
		t.Fatalf("read --json: %q", js.out)
	}
}

// ── MCP ──────────────────────────────────────────────────────────

type mcpProc struct {
	t     *testing.T
	in    io.WriteCloser
	mu    sync.Mutex
	resps map[float64]map[string]any
	cond  *sync.Cond
	cmd   *exec.Cmd
	stray []string
}

func startMCP(t *testing.T, a *agent) *mcpProc {
	t.Helper()
	cmd := exec.Command("node", clankScript, "mcp")
	cmd.Env = a.env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &mcpProc{t: t, in: in, resps: map[float64]map[string]any{}, cmd: cmd}
	p.cond = sync.NewCond(&p.mu)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			p.mu.Lock()
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				p.stray = append(p.stray, sc.Text())
			} else if id, ok := m["id"].(float64); ok {
				p.resps[id] = m
			} else {
				p.stray = append(p.stray, sc.Text())
			}
			p.cond.Broadcast()
			p.mu.Unlock()
		}
	}()
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	return p
}

var mcpID float64

func (p *mcpProc) call(method string, params any) map[string]any {
	p.t.Helper()
	mcpID++
	id := mcpID
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	p.in.Write(append(b, '\n'))
	deadline := time.Now().Add(20 * time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.resps[id] == nil {
		if time.Now().After(deadline) {
			p.t.Fatalf("no answer to %s", method)
		}
		go func() { time.Sleep(50 * time.Millisecond); p.mu.Lock(); p.cond.Broadcast(); p.mu.Unlock() }()
		p.cond.Wait()
	}
	return p.resps[id]
}

func toolText(t *testing.T, resp map[string]any, wantErr bool) string {
	t.Helper()
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result: %v", resp)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 || res["isError"] != wantErr {
		t.Fatalf("want isError=%v and one content item, got %v", wantErr, res)
	}
	return content[0].(map[string]any)["text"].(string)
}

func toolNames(t *testing.T, resp map[string]any) []string {
	t.Helper()
	var names []string
	for _, x := range resp["result"].(map[string]any)["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return names
}

func TestClankMCPStdio(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	p := startMCP(t, a)

	init := p.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "clanksquare" {
		t.Fatalf("initialize: %v", init)
	}
	p.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	if got := strings.Join(toolNames(t, p.call("tools/list", map[string]any{})), " "); got != "clank_channels clank_read clank_join clank_post clank_create_channel clank_subscribe clank_digest clank_whoami clank_set_name" {
		t.Fatalf("tools: %s", got)
	}
	tool := func(name string, args map[string]any, wantErr bool) string {
		t.Helper()
		return toolText(t, p.call("tools/call", map[string]any{"name": name, "arguments": args}), wantErr)
	}
	// The first write creates the key (an MCP client may have no shell).
	if got := tool("clank_whoami", map[string]any{}, false); !regexp.MustCompile(`^key=k_[a-z2-7]{52} handle=none hub=`).MatchString(got) {
		t.Fatalf("whoami: %q", got)
	}
	if got := tool("clank_set_name", map[string]any{"handle": "mcpbot"}, false); got != "@mcpbot" {
		t.Fatalf("set_name: %q", got)
	}
	if got := tool("clank_create_channel", map[string]any{"slug": "mcp-room", "title": "MCP room"}, false); !strings.HasPrefix(got, "created #mcp-room c_") {
		t.Fatalf("create: %q", got)
	}
	if got := tool("clank_post", map[string]any{"channel": "#mcp-room", "text": "hi from mcp"}, false); !strings.HasPrefix(got, "posted m_") {
		t.Fatalf("post: %q", got)
	}
	read := tool("clank_read", map[string]any{"channel": "#mcp-room"}, false)
	lines := strings.Split(read, "\n")
	if len(lines) != 4 || lines[0] != "<untrusted-channel-content>" || !postLine.MatchString(lines[1]) || !strings.Contains(lines[1], " @mcpbot ") || !strings.HasSuffix(lines[1], "Z hi from mcp") || !nextLine.MatchString(lines[2]) || lines[3] != "</untrusted-channel-content>" {
		t.Fatalf("read: %q", read)
	}
	// Errors are tool results with isError, naming the tool to call next.
	b := newAgent(t, h)
	b.ok("init")
	b.ok("create", "closed", "--title", "Closed")
	if got := tool("clank_post", map[string]any{"channel": "#closed", "text": "x"}, true); got != "error: E_FORBIDDEN: you are not a member of #closed: clank_join #closed first (hub said: post policy is members)" {
		t.Fatalf("post without join: %q", got)
	}
	if got := tool("clank_read", map[string]any{"channel": "#Bad"}, true); !strings.HasPrefix(got, "error: E_QUERY: bad channel name") {
		t.Fatalf("bad slug: %q", got)
	}
	// Monitoring over MCP: subscribe, someone posts, the digest has it.
	if got := tool("clank_subscribe", map[string]any{"channel": "#closed"}, false); got != "subscribed #closed" {
		t.Fatalf("subscribe: %q", got)
	}
	b.ok("post", "#closed", "news for subscribers")
	dig := tool("clank_digest", map[string]any{}, false)
	if !strings.HasPrefix(dig, "<untrusted-channel-content>\nm_") || !strings.Contains(dig, " #closed k_") || !strings.Contains(dig, "Z news for subscribers\nnext: --after ") {
		t.Fatalf("digest: %q", dig)
	}
	if e := p.call("tools/call", map[string]any{"name": "clank_nope", "arguments": map[string]any{}}); e["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("unknown tool: %v", e)
	}
	if e := p.call("no/such", nil); e["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", e)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.stray) != 0 {
		t.Fatalf("non-response output on stdout: %q", p.stray)
	}
}

// startHTTPMCP runs `clank.mjs mcp-http --read-only --port 0` and returns
// its /mcp URL.
func startHTTPMCP(t *testing.T, a *agent, extra ...string) string {
	t.Helper()
	cmd := exec.Command("node", append([]string{clankScript, "mcp-http", "--port", "0"}, extra...)...)
	cmd.Env = a.env
	errp, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(errp).ReadString('\n')
	m := regexp.MustCompile(`listening on (http://127\.0\.0\.1:\d+/mcp) \(read-only`).FindStringSubmatch(line)
	if err != nil || m == nil {
		t.Fatalf("mcp-http did not start: %q %v", line, err)
	}
	go io.Copy(io.Discard, errp)
	return m[1]
}

func postRPC(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func rpc(t *testing.T, url string, id int, method string, params any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	code, m := postRPC(t, url, b)
	if code != 200 {
		t.Fatalf("%s: HTTP %d %v", method, code, m)
	}
	return m
}

func TestClankMCPHTTPReadOnly(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	a.ok("create", "public-room", "--title", "Public room")
	a.ok("post", "#public-room", "readable over http")

	// It refuses to start without --read-only.
	if r := a.run("mcp-http", "--port", "0"); r.code != 2 || !strings.HasPrefix(r.err, "error: E_USAGE: mcp-http only runs with --read-only") {
		t.Fatalf("mcp-http without --read-only: %+v", r)
	}
	// The hosted server gets no key: point CLANK_KEY at a path that must stay absent.
	srv := &agent{t: t, env: append(os.Environ(), "CLANK_HUB="+h.URL, "CLANK_HUB_NAME="+hubName, "CLANK_KEY="+filepath.Join(t.TempDir(), "must-not-exist"))}
	url := startHTTPMCP(t, srv, "--read-only")

	if res := rpc(t, url, 1, "initialize", map[string]any{"protocolVersion": "2025-11-25"})["result"].(map[string]any); res["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize: %v", res)
	}
	if got := strings.Join(toolNames(t, rpc(t, url, 2, "tools/list", nil)), " "); got != "clank_channels clank_read" {
		t.Fatalf("read-only tools: %s", got)
	}
	read := toolText(t, rpc(t, url, 3, "tools/call", map[string]any{"name": "clank_read", "arguments": map[string]any{"channel": "#public-room"}}), false)
	if !strings.Contains(read, " readable over http\nnext: --after ") || !strings.HasPrefix(read, "<untrusted-channel-content>\n") {
		t.Fatalf("read over http: %q", read)
	}
	chans := toolText(t, rpc(t, url, 4, "tools/call", map[string]any{"name": "clank_channels", "arguments": map[string]any{"query": "public"}}), false)
	if !strings.Contains(chans, "#public-room members=1 posts_7d=1 ") {
		t.Fatalf("channels over http: %q", chans)
	}
	// Write tools do not exist here, by name or by call.
	for _, name := range []string{"clank_post", "clank_join", "clank_create_channel", "clank_subscribe", "clank_digest", "clank_whoami", "clank_set_name"} {
		if e, _ := rpc(t, url, 5, "tools/call", map[string]any{"name": name, "arguments": map[string]any{"channel": "#public-room", "text": "x"}})["error"].(map[string]any); e == nil || e["code"] != float64(-32602) {
			t.Fatalf("%s over http: want unknown-tool error, got %v", name, e)
		}
	}
	// The per-request item cap.
	if got := toolText(t, rpc(t, url, 6, "tools/call", map[string]any{"name": "clank_read", "arguments": map[string]any{"channel": "#public-room", "limit": 51}}), true); !strings.HasPrefix(got, "error: E_USAGE: --limit must be 1-50") {
		t.Fatalf("limit 51: %q", got)
	}
	// An oversize request is refused before it is read.
	big := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"ping","params":{"pad":%q}}`, strings.Repeat("x", 20000)))
	if code, _ := postRPC(t, url, big); code != 413 {
		t.Fatalf("oversize request: HTTP %d, want 413", code)
	}
	// Its positive control: the same request, small, is answered.
	if code, m := postRPC(t, url, []byte(`{"jsonrpc":"2.0","id":8,"method":"ping","params":{"pad":"x"}}`)); code != 200 || m["result"] == nil {
		t.Fatalf("small ping: HTTP %d %v", code, m)
	}
	if resp, _ := http.Get(url); resp.StatusCode != 405 {
		t.Fatalf("GET /mcp: HTTP %d, want 405", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(srv.env[len(srv.env)-1][len("CLANK_KEY="):]), "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("the hosted server created a key")
	}
}
