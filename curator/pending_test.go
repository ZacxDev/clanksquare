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
