package curator

import (
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

func TestR1RefusedPendingIsDropped(t *testing.T) {
	// Invariant guard (7e412eb never wrote pending records): a pending post
	// the hub definitively refuses (E_STALE: it was never stored) is dropped,
	// and the item is then posted once.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run()
	cid := w.hub.slugs["news"]
	key := testKey("curator")
	old := clankmail.Sign(key, msgPayload(key, cid, "1", "https://x.example/1\n\nvia A", testNow.Add(-time.Hour).UnixMilli()))
	st, _ := LoadState(w.statePath())
	st.Channels[cid].Pending = map[string]string{"https://x.example/1": old}
	st.Save(w.statePath())
	w.hub.staleBefore = testNow.Add(-10 * time.Minute).UnixMilli()
	oneItem(w, "1", "https://x.example/1", 1)
	res := w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
		t.Fatalf("log is %v, want the item once", got)
	}
	if res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
}

func TestPendingNotDroppedOnAmbiguous4xx(t *testing.T) {
	// The post is stored but its answer is lost (5xx), so it stays pending.
	// The end-of-run resend then gets a 4xx that does NOT mean "not stored":
	// a proxy's bare 404, or a hub §4 code (a truncated body fails before the
	// stored-id check). Dropping the record there reposts next run.
	cases := map[string]storedAnswer{
		"proxy-404":    {404, ""},
		"hub-encoding": {400, "E_ENCODING"},
	}
	for name, ans := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.feeds.set("/a", atomFeedXML("A"))
			w.config(5, feed("/a"))
			w.run() // channel only
			oneItem(w, "1", "https://x.example/1", 1)
			w.hub.mu.Lock()
			w.hub.breakAfterStore = true
			w.hub.storedAnswers = []storedAnswer{ans}
			w.hub.mu.Unlock()
			w.now = w.now.Add(time.Minute)
			if res := w.run(); res.ExitCode() == 0 {
				t.Error("an unsettled pending post must fail the run")
			}
			w.now = w.now.Add(time.Hour)
			w.run()
			if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
				t.Fatalf("log is %v, want the item once", got)
			}
		})
	}
}
