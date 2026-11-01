package curator

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// fakeHub is a small in-memory stand-in for go/hub, enough for the curator:
// submit (handle, channel genesis/update, msg to a channel), channel lookup,
// paged logs, handles and objects. It verifies what it is sent with the
// reference pipeline. The real hub is exercised by go/hub's curator e2e test.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	objects  map[string]string // id -> token
	slugs    map[string]string // slug -> c_id
	genesis  map[string]string // c_id -> genesis token
	manifest map[string]string // c_id -> current manifest token
	logs     map[string][]string
	handles  map[string]string // name -> handle token
	submits  []string          // every accepted (new) object's type, in order

	// rateAfter > 0: submissions after this many accepted ones get E_RATE.
	rateAfter int
	// breakAfterStore: the next submit is stored, then answered with a 502.
	breakAfterStore bool
	// wrongID: answer every submit with a different id.
	wrongID bool
	// objects502: the next n GET /v1/objects answer 502.
	objects502 int
	// down: every submit is answered 502; a new token is still stored
	// first (the hub stored it, the answer was lost).
	down bool
	// staleBefore > 0: a new (unstored) token with ts below it is E_STALE.
	staleBefore int64
	// plain429After > 0: submissions after this many accepted ones get a
	// bare 429 with an HTML body and no ClankMail error code (a relay's).
	plain429After int
	// onStore, when set, runs after a new msg is stored and before the
	// answer is written (the hub mutex is not held).
	onStore func(obj *clankmail.Object)
	// postSubmits counts every msg submission received, stored or not.
	postSubmits int
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t, objects: map[string]string{}, slugs: map[string]string{}, genesis: map[string]string{},
		manifest: map[string]string{}, logs: map[string][]string{}, handles: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/submit", h.handleSubmit)
	mux.HandleFunc("GET /v1/channels/{ref}", h.handleChannel)
	mux.HandleFunc("GET /v1/channels/{id}/log", h.handleLog)
	mux.HandleFunc("GET /v1/handles/{name}", h.handleHandle)
	mux.HandleFunc("GET /v1/objects/{id}", h.handleObject)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) setOnStore(fn func(*clankmail.Object)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onStore = fn
}

func hubErr(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "detail": detail})
}

func okJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *fakeHub) handleSubmit(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	tok := strings.TrimSpace(string(raw))
	obj, err := clankmail.Verify(tok)
	if err != nil {
		hubErr(w, 400, err.(*clankmail.Error).Code, err.Error())
		return
	}
	h.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			h.mu.Unlock()
		}
	}()
	if obj.Type == "msg" {
		h.postSubmits++
	}
	if _, ok := h.objects[obj.ID]; ok {
		if h.down {
			hubErr(w, 502, "", "bad gateway")
			return
		}
		okJSON(w, map[string]string{"id": obj.ID}) // idempotent
		return
	}
	if h.staleBefore > 0 && obj.Fields["ts"].(int64) < h.staleBefore {
		hubErr(w, 400, "E_STALE", "ts outside the window")
		return
	}
	if h.plain429After > 0 && len(h.submits) >= h.plain429After {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(429)
		io.WriteString(w, "<html><body>Too Many Requests</body></html>")
		return
	}
	if h.rateAfter > 0 && len(h.submits) >= h.rateAfter {
		hubErr(w, 429, "E_RATE", "rate limit exceeded (signer key)")
		return
	}
	f := obj.Fields
	switch obj.Type {
	case "handle":
		if cur, ok := h.handles[f["name"].(string)]; ok {
			if prev, _ := clankmail.Verify(cur); prev.From != obj.From {
				hubErr(w, 403, "E_FORBIDDEN", "name taken")
				return
			}
		}
		h.handles[f["name"].(string)] = tok
	case "channel":
		slug := f["slug"].(string)
		if cid, isUpdate := f["channel"].(string); isUpdate {
			g, ok := h.genesis[cid]
			if !ok {
				hubErr(w, 404, "E_NOT_FOUND", "unknown channel")
				return
			}
			gen, _ := clankmail.Verify(g)
			cur, _ := clankmail.Verify(h.manifest[cid])
			if obj.From != gen.From || f["ts"].(int64) <= cur.Fields["ts"].(int64) {
				hubErr(w, 403, "E_FORBIDDEN", "not owner or not newer")
				return
			}
			h.manifest[cid] = tok
		} else {
			if _, taken := h.slugs[slug]; taken {
				hubErr(w, 403, "E_FORBIDDEN", "slug bound")
				return
			}
			cid := clankmail.ChannelID(obj.Payload)
			h.slugs[slug], h.genesis[cid], h.manifest[cid] = cid, tok, tok
		}
	case "msg":
		to := strList(f["to"])
		if len(to) != 1 || h.genesis[to[0]] == "" {
			hubErr(w, 404, "E_NOT_FOUND", "unknown channel")
			return
		}
		// post: members — the owner is a member from genesis (as in go/hub).
		gen, _ := clankmail.Verify(h.genesis[to[0]])
		if obj.From != gen.From {
			hubErr(w, 403, "E_FORBIDDEN", "post policy is members")
			return
		}
		h.logs[to[0]] = append(h.logs[to[0]], tok)
	default:
		hubErr(w, 403, "E_FORBIDDEN", "unsupported in fake hub")
		return
	}
	h.objects[obj.ID] = tok
	h.submits = append(h.submits, obj.Type)
	if obj.Type == "msg" && h.onStore != nil {
		hook := h.onStore
		h.mu.Unlock()
		unlocked = true
		hook(obj)
		h.mu.Lock()
		unlocked = false
	}
	if h.down {
		hubErr(w, 502, "", "bad gateway")
		return
	}
	if h.breakAfterStore {
		h.breakAfterStore = false
		hubErr(w, 502, "", "bad gateway")
		return
	}
	id := obj.ID
	if h.wrongID {
		id = "m_" + strings.Repeat("a", 52)
	}
	okJSON(w, map[string]string{"id": id})
}

func (h *fakeHub) handleChannel(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ref := r.PathValue("ref")
	cid := ref
	if s, ok := strings.CutPrefix(ref, "#"); ok {
		cid = h.slugs[s]
	}
	if h.genesis[cid] == "" {
		hubErr(w, 404, "E_NOT_FOUND", "no such channel")
		return
	}
	okJSON(w, map[string]string{"channel": cid, "manifest": h.manifest[cid], "genesis": h.genesis[cid]})
}

func (h *fakeHub) handleLog(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cid := r.PathValue("id")
	if h.genesis[cid] == "" {
		hubErr(w, 404, "E_NOT_FOUND", "no such channel")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	log := h.logs[cid]
	type item struct {
		Cursor string `json:"cursor"`
		Token  string `json:"token"`
	}
	items := []item{}
	next := after
	for i := after; i < len(log) && len(items) < limit; i++ {
		items = append(items, item{strconv.Itoa(i + 1), log[i]})
		next = i + 1
	}
	okJSON(w, map[string]any{"items": items, "next": strconv.Itoa(next)})
}

func (h *fakeHub) handleHandle(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tok, ok := h.handles[r.PathValue("name")]
	if !ok {
		hubErr(w, 404, "E_NOT_FOUND", "no such handle")
		return
	}
	okJSON(w, map[string]any{"handle": tok, "agents": []any{}})
}

func (h *fakeHub) handleObject(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.objects502 > 0 {
		h.objects502--
		hubErr(w, 502, "", "bad gateway")
		return
	}
	id := r.PathValue("id")
	tok, ok := h.objects[id]
	if !ok {
		hubErr(w, 404, "E_NOT_FOUND", "no such object")
		return
	}
	okJSON(w, map[string]string{"id": id, "token": tok})
}

// posts returns the decoded msg posts in a channel's log, by slug.
func (h *fakeHub) posts(slug string) []*clankmail.Object {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*clankmail.Object
	for _, tok := range h.logs[h.slugs[slug]] {
		obj, err := clankmail.Verify(tok)
		if err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// firstLines returns the first body line of every post in a channel.
func (h *fakeHub) firstLines(slug string) []string {
	var out []string
	for _, p := range h.posts(slug) {
		line, _, _ := strings.Cut(p.Fields["body"].(string), "\n")
		out = append(out, line)
	}
	return out
}

func (h *fakeHub) count(typ string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.submits {
		if s == typ {
			n++
		}
	}
	return n
}

// appendLog puts a raw token into a channel's log as a (possibly hostile)
// hub would serve it, bypassing every check.
func (h *fakeHub) appendLog(slug, tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cid := h.slugs[slug]
	h.logs[cid] = append(h.logs[cid], tok)
}

// testKey derives a fixed key from a name.
func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("clankmail-curator-test/" + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func keyID(priv ed25519.PrivateKey) string {
	return clankmail.KeyID(priv.Public().(ed25519.PublicKey))
}

// feedServer serves mutable feed bodies by path; a path with no body is 404.
type feedServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	bod map[string]string
}

func newFeedServer(t *testing.T) *feedServer {
	fs := &feedServer{bod: map[string]string{}}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		b, ok := fs.bod[r.URL.Path]
		fs.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		io.WriteString(w, b)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *feedServer) set(path, body string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.bod[path] = body
}

func (fs *feedServer) url(path string) string { return fs.srv.URL + path }

// entry is one Atom entry for atomFeedXML.
type entry struct {
	id, link, title, summary, date string // date "" = undated
}

func atomFeedXML(title string, es ...entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom"><title>%s</title>`, title)
	for _, e := range es {
		b.WriteString("<entry>")
		fmt.Fprintf(&b, "<id>%s</id><title>%s</title>", e.id, e.title)
		if e.link != "" {
			fmt.Fprintf(&b, `<link rel="alternate" href="%s"/>`, e.link)
		}
		if e.date != "" {
			fmt.Fprintf(&b, "<updated>%s</updated>", e.date)
		}
		if e.summary != "" {
			fmt.Fprintf(&b, `<summary type="html">%s</summary>`, e.summary)
		}
		b.WriteString("</entry>")
	}
	b.WriteString("</feed>")
	return b.String()
}
