package recommender

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// fakeHub is an in-memory stand-in for the reference hub, enough for the
// recommender: submit (channel genesis/update, msg to a channel, with the
// post policy), channel lookup by #slug or c_ id, paged logs, discovery
// (sort new/active, paged) and objects. The real hub is exercised by e2e/.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	channels map[string]*fakeChannel // c_id
	slugs    map[string]string
	objects  map[string]string
	seq      int
	submits  []string // type of every accepted new object, in order
	// rateAfterMsgs > 0: msg submissions after this many accepted msgs get 429 E_RATE.
	rateAfterMsgs int
	msgs          int
}

type fakeChannel struct {
	id, genesis, manifest string
	created               int64
	posts7d, members      int
	log                   []logEntry
}

type logEntry struct {
	cursor int
	token  string
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t, channels: map[string]*fakeChannel{}, slugs: map[string]string{}, objects: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/submit", h.handleSubmit)
	mux.HandleFunc("GET /v1/channels", h.handleDiscovery)
	mux.HandleFunc("GET /v1/channels/{ref}", h.handleChannel)
	mux.HandleFunc("GET /v1/channels/{id}/log", h.handleLog)
	mux.HandleFunc("GET /v1/objects/{id}", h.handleObject)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func hubErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "detail": "fake"})
}

func okJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *fakeHub) handleSubmit(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	tok := strings.TrimSpace(string(raw))
	code, status := h.store(tok)
	if code != "" {
		hubErr(w, status, code)
		return
	}
	okJSON(w, map[string]string{"id": hubkit.TokenID(tok)})
}

// store applies a submission; it returns an error code and status, or "".
func (h *fakeHub) store(tok string) (string, int) {
	obj, err := clankmail.Verify(tok)
	if err != nil {
		return err.(*clankmail.Error).Code, 400
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.objects[obj.ID]; ok {
		return "", 0 // idempotent
	}
	f := obj.Fields
	switch obj.Type {
	case "channel":
		slug := f["slug"].(string)
		if cid, isUpdate := f["channel"].(string); isUpdate {
			ch := h.channels[cid]
			if ch == nil {
				return "E_NOT_FOUND", 404
			}
			gen, _ := clankmail.Verify(ch.genesis)
			cur, _ := clankmail.Verify(ch.manifest)
			if obj.From != gen.From || f["ts"].(int64) <= cur.Fields["ts"].(int64) {
				return "E_FORBIDDEN", 403
			}
			ch.manifest = tok
		} else {
			if _, taken := h.slugs[slug]; taken {
				return "E_FORBIDDEN", 403
			}
			cid := clankmail.ChannelID(obj.Payload)
			h.slugs[slug] = cid
			h.channels[cid] = &fakeChannel{id: cid, genesis: tok, manifest: tok, created: f["ts"].(int64), members: 1}
		}
	case "msg":
		to := hubkit.StrList(f["to"])
		if len(to) != 1 || h.channels[to[0]] == nil {
			return "E_NOT_FOUND", 404
		}
		ch := h.channels[to[0]]
		if ch == nil {
			return "E_NOT_FOUND", 404
		}
		if h.rateAfterMsgs > 0 && h.msgs >= h.rateAfterMsgs {
			return "E_RATE", 429
		}
		man, _ := clankmail.Verify(ch.manifest)
		gen, _ := clankmail.Verify(ch.genesis)
		if man.Fields["post"] == "owner" && obj.From != gen.From {
			return "E_FORBIDDEN", 403
		}
		h.msgs++
		h.seq++
		ch.log = append(ch.log, logEntry{h.seq, tok})
		ch.posts7d++
	default:
		return "E_FORBIDDEN", 403
	}
	h.objects[obj.ID] = tok
	h.submits = append(h.submits, obj.Type)
	return "", 0
}

func (h *fakeHub) handleChannel(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ref := r.PathValue("ref")
	cid := ref
	if s, ok := strings.CutPrefix(ref, "#"); ok {
		cid = h.slugs[s]
	}
	ch := h.channels[cid]
	if ch == nil {
		hubErr(w, 404, "E_NOT_FOUND")
		return
	}
	okJSON(w, map[string]string{"channel": cid, "manifest": ch.manifest, "genesis": ch.genesis})
}

func (h *fakeHub) handleLog(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channels[r.PathValue("id")]
	if ch == nil {
		hubErr(w, 404, "E_NOT_FOUND")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items := []map[string]string{}
	next := ""
	for _, e := range ch.log {
		if e.cursor > after && len(items) < limit {
			items = append(items, map[string]string{"cursor": strconv.Itoa(e.cursor), "token": e.token})
			next = strconv.Itoa(e.cursor)
		}
	}
	okJSON(w, map[string]any{"items": items, "next": next})
}

func (h *fakeHub) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := r.URL.Query()
	var all []*fakeChannel
	for _, ch := range h.channels {
		all = append(all, ch)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if q.Get("sort") == "active" && a.posts7d != b.posts7d {
			return a.posts7d > b.posts7d
		}
		if a.created != b.created {
			return a.created > b.created
		}
		return a.id < b.id
	})
	off, _ := strconv.Atoi(q.Get("after"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	end := min(off+limit, len(all))
	items := []map[string]any{}
	for _, ch := range all[min(off, len(all)):end] {
		items = append(items, map[string]any{"channel": ch.id, "manifest": ch.manifest, "members": ch.members, "posts_7d": ch.posts7d})
	}
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	okJSON(w, map[string]any{"items": items, "next": next})
}

func (h *fakeHub) handleObject(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tok, ok := h.objects[r.PathValue("id")]
	if !ok {
		hubErr(w, 404, "E_NOT_FOUND")
		return
	}
	okJSON(w, map[string]string{"id": r.PathValue("id"), "token": tok})
}

// recs is every recommendation in #slug, in log order, parsed.
func (h *fakeHub) recs(slug string) []Rec {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channels[h.slugs[slug]]
	if ch == nil {
		return nil
	}
	var out []Rec
	for _, e := range ch.log {
		obj, _ := clankmail.Verify(e.token)
		rec, err := ParseBody(obj.Fields["body"].(string))
		if err != nil {
			h.t.Fatalf("the recommender posted an unparseable body: %v\n%s", err, obj.Fields["body"])
		}
		out = append(out, rec)
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

func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("recommender test key " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// addChannel creates a channel owned by owner, created at ts (ms).
func (h *fakeHub) addChannel(owner ed25519.PrivateKey, slug, title, about string, tags []string, ts int64, post string) string {
	h.t.Helper()
	payload := hubkit.Encode(struct {
		hubkit.Common
		Slug  string   `json:"slug"`
		Title string   `json:"title"`
		About string   `json:"about,omitempty"`
		Tags  []string `json:"tags,omitempty"`
		Read  string   `json:"read"`
		Post  string   `json:"post"`
		Join  string   `json:"join"`
	}{hubkit.Header(owner, "channel", ts), slug, title, about, tags, "public", post, "open"})
	if code, _ := h.store(clankmail.Sign(owner, payload)); code != "" {
		h.t.Fatalf("addChannel %s: %s", slug, code)
	}
	return clankmail.ChannelID(payload)
}

func (h *fakeHub) setActivity(id string, posts7d int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.channels[id].posts7d = posts7d
}

// encodeUpdate is a manifest update of channel cid.
func encodeUpdate(owner ed25519.PrivateKey, cid, slug, title string, ts int64) []byte {
	return hubkit.Encode(struct {
		hubkit.Common
		Channel string `json:"channel"`
		Slug    string `json:"slug"`
		Title   string `json:"title"`
		Read    string `json:"read"`
		Post    string `json:"post"`
		Join    string `json:"join"`
	}{hubkit.Header(owner, "channel", ts), cid, slug, title, "public", "members", "open"})
}
