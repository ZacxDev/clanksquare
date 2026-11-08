package editor

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// ---- fake hub ----

// fakeHub is an in-memory stand-in for the reference hub with what the
// editor uses: submit (channel genesis/update, msg), channel lookup, paged
// logs and objects. It verifies what it is sent with the reference package.
type fakeHub struct {
	srv *httptest.Server

	mu       sync.Mutex
	objects  map[string]string
	slugs    map[string]string
	genesis  map[string]string
	manifest map[string]string
	logs     map[string][]string
	submits  []string // every request to /v1/submit, by object type

	// breakAfterStore: the next new msg is stored, then answered 502.
	breakAfterStore bool
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{objects: map[string]string{}, slugs: map[string]string{}, genesis: map[string]string{},
		manifest: map[string]string{}, logs: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/submit", h.handleSubmit)
	mux.HandleFunc("GET /v1/channels/{ref}", h.handleChannel)
	mux.HandleFunc("GET /v1/channels/{id}/log", h.handleLog)
	mux.HandleFunc("GET /v1/objects/{id}", h.handleObject)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
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
		hubErr(w, 400, "E_SIG", err.Error())
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.submits = append(h.submits, obj.Type)
	if _, ok := h.objects[obj.ID]; ok {
		okJSON(w, map[string]string{"id": obj.ID})
		return
	}
	f := obj.Fields
	switch obj.Type {
	case "channel":
		slug := f["slug"].(string)
		if cid, isUpdate := f["channel"].(string); isUpdate {
			gen, _ := clankmail.Verify(h.genesis[cid])
			if gen == nil || obj.From != gen.From {
				hubErr(w, 403, "E_FORBIDDEN", "not owner")
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
		to := hubkit.StrList(f["to"])
		if len(to) != 1 || h.genesis[to[0]] == "" {
			hubErr(w, 404, "E_NOT_FOUND", "unknown channel")
			return
		}
		gen, _ := clankmail.Verify(h.genesis[to[0]])
		if obj.From != gen.From {
			hubErr(w, 403, "E_FORBIDDEN", "post policy")
			return
		}
		h.logs[to[0]] = append(h.logs[to[0]], tok)
	default:
		hubErr(w, 403, "E_FORBIDDEN", "unsupported")
		return
	}
	h.objects[obj.ID] = tok
	if obj.Type == "msg" && h.breakAfterStore {
		h.breakAfterStore = false
		hubErr(w, 502, "", "bad gateway")
		return
	}
	okJSON(w, map[string]string{"id": obj.ID})
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
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 50
	}
	type item struct {
		Cursor string `json:"cursor"`
		Token  string `json:"token"`
	}
	items := []item{}
	next := after
	for i := after; i < len(h.logs[cid]) && len(items) < limit; i++ {
		items = append(items, item{strconv.Itoa(i + 1), h.logs[cid][i]})
		next = i + 1
	}
	okJSON(w, map[string]any{"items": items, "next": strconv.Itoa(next)})
}

func (h *fakeHub) handleObject(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := r.PathValue("id")
	tok, ok := h.objects[id]
	if !ok {
		hubErr(w, 404, "E_NOT_FOUND", "no such object")
		return
	}
	okJSON(w, map[string]string{"id": id, "token": tok})
}

// mustSubmit stores a token directly (setup), as an accepted submit would.
func (h *fakeHub) mustSubmit(t *testing.T, tok string) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/v1/submit", "text/plain", strings.NewReader(tok))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("setup submit failed: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	h.mu.Lock()
	h.submits = nil // setup does not count
	h.mu.Unlock()
}

// appendLog puts a token into a channel's log bypassing every check.
func (h *fakeHub) appendLog(slug, tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs[h.slugs[slug]] = append(h.logs[h.slugs[slug]], tok)
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

func (h *fakeHub) posts(slug string) []*clankmail.Object {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*clankmail.Object
	for _, tok := range h.logs[h.slugs[slug]] {
		if obj, err := clankmail.Verify(tok); err == nil {
			out = append(out, obj)
		}
	}
	return out
}

func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("clanksquare-editor-test/" + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// rawChannel creates a curator-style raw channel owned by owner.
func rawChannel(t *testing.T, h *fakeHub, owner ed25519.PrivateKey, slug string, ts int64) string {
	t.Helper()
	p := hubkit.Encode(struct {
		hubkit.Common
		Slug  string `json:"slug"`
		Title string `json:"title"`
		Read  string `json:"read"`
		Post  string `json:"post"`
		Join  string `json:"join"`
	}{hubkit.Header(owner, "channel", ts), slug, slug, "public", "members", "open"})
	h.mustSubmit(t, clankmail.Sign(owner, p))
	return clankmail.ChannelID(p)
}

// rawPost appends a curator-format post to a raw channel.
func rawPost(t *testing.T, h *fakeHub, key ed25519.PrivateKey, channelID, title, link, excerpt string, ts int64) string {
	t.Helper()
	body := link + "\n\n" + excerpt + "\n\nvia arXiv (cs.AI, cs.CL)"
	tok := clankmail.Sign(key, hubkit.MsgPayload(key, channelID, title, body, ts))
	h.mu.Lock()
	h.logs[channelID] = append(h.logs[channelID], tok)
	h.objects[hubkit.TokenID(tok)] = tok
	h.mu.Unlock()
	return hubkit.TokenID(tok)
}

// ---- fake OpenRouter ----

// fakeOR serves /api/alpha/decisions: probs[title] (default drop-heavy).
type fakeOR struct {
	srv *httptest.Server

	mu       sync.Mutex
	probs    map[string]map[string]float64
	requests map[string]int
	bodies   []map[string]any
	cost     float64 // usage.cost on every answer
}

func newFakeOR(t *testing.T) *fakeOR {
	f := &fakeOR{probs: map[string]map[string]float64{}, requests: map[string]int{}, cost: 0.0001}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/alpha/decisions", f.decisions)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOR) read(r *http.Request, path string) map[string]any {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[path]++
	f.bodies = append(f.bodies, body)
	return body
}

func (f *fakeOR) decisions(w http.ResponseWriter, r *http.Request) {
	body := f.read(r, "decisions")
	f.mu.Lock()
	defer f.mu.Unlock()
	st, _ := body["state"].(map[string]any)
	text, ok := st["text"].(string)
	if !ok {
		hubErr(w, 400, "", "ScaleDown state must be a string or an object with only text")
		return
	}
	var state map[string]any
	json.Unmarshal([]byte(text), &state)
	answers := map[string]any{}
	for k, q := range body["questions"].(map[string]any) {
		switch q.(map[string]any)["type"] {
		case "choice":
			title := ""
			if s, ok := state["story"].(map[string]any); ok {
				title, _ = s["title"].(string)
			}
			p, ok := f.probs[title]
			if !ok {
				p = map[string]float64{"post": 0.1, "digest": 0.2, "drop": 0.7}
			}
			best, bp := "", -1.0
			for _, l := range Labels {
				if p[l] > bp {
					best, bp = l, p[l]
				}
			}
			answers[k] = map[string]any{"type": "choice", "choice": best, "probabilities": p, "confidence": bp}
		}
	}
	okJSON(w, map[string]any{"model": "scaledown/classify-1-20260928", "answers": answers, "usage": map[string]any{"input_tokens": 500, "cost": f.cost}})
}

func (f *fakeOR) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func (f *fakeOR) model(capUSD float64) *Model {
	m, _ := NewMeter(capUSD, "")
	return &Model{Base: f.srv.URL, Key: "test-key", HTTP: f.srv.Client(), Meter: m, Prices: DefaultPrices}
}
