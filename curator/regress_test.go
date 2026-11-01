package curator

// Regression tests for the PR #9 round-1 audit findings (R1, R2, 🟡1–🟡5,
// 🟢). Each was watched failing against 7e412eb before its fix.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

func oneItem(w *world, id, link string, hoursAgo float64) {
	w.feeds.set("/a", atomFeedXML("A", entry{id: id, link: link, title: id, date: ago(hoursAgo)}))
}

// ---- R1: an unconfirmed submit must not be forgotten ----

func TestR1UnconfirmedSubmitNotForgotten(t *testing.T) {
	// The hub stores the post but the answer is lost, and the confirming
	// lookup fails too (a relay 502). The post must not be made again.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run() // channel only, state on disk
	oneItem(w, "1", "https://x.example/1", 1)
	w.hub.breakAfterStore = true
	w.hub.objects502 = 1
	w.now = w.now.Add(time.Minute)
	w.run()
	w.now = w.now.Add(time.Hour)
	w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
		t.Fatalf("log is %v, want the item exactly once", got)
	}
}

func TestR1CancelledRunNotForgotten(t *testing.T) {
	// SIGTERM (signal.NotifyContext) cancels the run's context right after
	// the hub stored the post. The next run must not post it again.
	w := newWorld(t)
	oneItem(w, "0", "https://x.example/0", 2)
	w.config(5, feed("/a"))
	w.run() // channel + item 0, state on disk
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "0", link: "https://x.example/0", title: "0", date: ago(2)},
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)},
	))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.hub.setOnStore(func(*clankmail.Object) { cancel(); time.Sleep(50 * time.Millisecond) })
	w.now = w.now.Add(time.Minute)
	w.run()
	w.ctx = nil
	w.hub.setOnStore(nil)
	// The cancelled run itself settles the post, by resending it under a
	// fresh context, rather than leaving it for the next run.
	if st, _ := LoadState(w.statePath()); st == nil || st.Channels[w.hub.slugs["news"]].Seen["https://x.example/1"] == 0 {
		t.Fatal("after the cancelled run the post is not recorded as seen")
	}
	w.now = w.now.Add(time.Hour)
	w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/0", "https://x.example/1"}) {
		t.Fatalf("log is %v, want each item exactly once", got)
	}
}

// TestCuratorChild is the body of the crash test's child process.
func TestCuratorChild(t *testing.T) {
	spec := os.Getenv("CURATOR_CHILD_SPEC")
	if spec == "" {
		t.Skip("helper for TestR1CrashBetweenSubmitAndSave")
	}
	var s struct {
		Config    Config
		StatePath string
		NowMS     int64
	}
	raw, _ := os.ReadFile(spec)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	Run(context.Background(), Options{Config: &s.Config, Key: testKey("curator"), StatePath: s.StatePath,
		Now: func() time.Time { return time.UnixMilli(s.NowMS) }, Out: os.Stderr})
}

func TestR1CrashBetweenSubmitAndSave(t *testing.T) {
	// SIGKILL after the hub stored a post and before the curator could
	// record it: the next run must not post it again.
	w := newWorld(t)
	oneItem(w, "0", "https://x.example/0", 2)
	w.config(5, feed("/a"))
	w.run() // channel + item 0, state on disk
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "0", link: "https://x.example/0", title: "0", date: ago(2)},
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(1)},
	))
	w.now = w.now.Add(time.Minute)
	spec := filepath.Join(w.dir, "child.json")
	raw, _ := json.Marshal(map[string]any{"Config": w.cfg, "StatePath": w.statePath(), "NowMS": w.now.UnixMilli()})
	os.WriteFile(spec, raw, 0o600)

	cmd := exec.Command(os.Args[0], "-test.run=^TestCuratorChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CURATOR_CHILD_SPEC="+spec)
	var killed atomic.Bool
	done := make(chan struct{})
	proc := make(chan *os.Process, 1)
	w.hub.setOnStore(func(*clankmail.Object) {
		// The child is blocked waiting for this answer: kill it by its own
		// PID and wait until it is gone before answering.
		if killed.CompareAndSwap(false, true) {
			p := <-proc
			p.Kill()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
			}
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc <- cmd.Process
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("child did not finish")
	}
	if !killed.Load() {
		t.Fatal("the child never submitted a post; the test is broken")
	}
	w.hub.setOnStore(nil)
	w.now = w.now.Add(time.Hour)
	w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/0", "https://x.example/1"}) {
		t.Fatalf("log is %v, want each item exactly once", got)
	}
}

func TestR1UnresolvedPendingBlocksChannel(t *testing.T) {
	// While the hub cannot say whether a post was stored, nothing new is
	// posted to that channel; once it can, the post is confirmed, not
	// repeated, and new items flow again.
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run() // channel only
	oneItem(w, "1", "https://x.example/1", 2)
	w.hub.down = true
	w.hub.objects502 = 1000
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() == 0 {
		t.Error("an unconfirmed post must fail the run")
	}
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(2)},
		entry{id: "2", link: "https://x.example/2", title: "2", date: ago(1)},
	))
	before := w.hub.postSubmits
	w.now = w.now.Add(time.Hour)
	w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
		t.Fatalf("while unresolved the log became %v, want only the first post", got)
	}
	if w.hub.postSubmits-before > 1 {
		t.Errorf("%d post submissions while a post was unresolved, want at most the one resend", w.hub.postSubmits-before)
	}
	w.hub.down, w.hub.objects502 = false, 0
	w.now = w.now.Add(time.Hour)
	if res := w.run(); res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1", "https://x.example/2"}) {
		t.Fatalf("after recovery the log is %v", got)
	}
}

// ---- R2: posted links are never forgotten ----

func TestR2RedatedItemDoesNotRepost(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%v", rebuild), func(t *testing.T) {
			// A GitHub release edited 31 days after it was posted comes back
			// with a fresh <updated> and is inside the window again.
			w := newWorld(t)
			oneItem(w, "1", "https://x.example/1", 1)
			w.config(5, feed("/a"))
			w.run()
			w.now = w.now.Add(31 * 24 * time.Hour)
			w.feeds.set("/a", atomFeedXML("A", entry{id: "1", link: "https://x.example/1", title: "1",
				date: w.now.Add(-time.Hour).Format(time.RFC3339)}))
			if rebuild {
				os.Remove(w.statePath())
			}
			w.run()
			if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
				t.Fatalf("log is %v: a re-dated item was posted again", got)
			}
		})
	}
}

// ---- 🟡2: the dedupe key is always derived from the posted first line ----

func TestY2GuidKeyMatchesRebuild(t *testing.T) {
	cases := map[string]string{
		"del":      "https://x.example/a&#127;",                         // DEL: not a URL as given, one after PlainText
		"long-url": "https://x.example/" + strings.Repeat("a", maxLink), // clipped to a valid URL
	}
	for name, guid := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.feeds.set("/a", atomFeedXML("A", entry{id: guid, title: "g", date: ago(1)}))
			w.config(5, feed("/a"))
			w.run()
			if n := len(w.hub.posts("news")); n != 1 {
				t.Fatalf("run 1 made %d posts, want 1", n)
			}
			os.Remove(w.statePath())
			w.now = w.now.Add(time.Minute)
			w.run()
			if n := len(w.hub.posts("news")); n != 1 {
				t.Fatalf("after a rebuild the log holds %d posts of one item", n)
			}
		})
	}
}

// ---- 🟡3: the rebuild does not hold the whole log ----

func TestY3RebuildMemoryBounded(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run()
	cid := w.hub.slugs["news"]
	// 1000 maximum-size posts by someone else (channels are join: open).
	other := testKey("other")
	big := strings.Repeat("x", 32768-40)
	for i := 0; i < 1000; i++ {
		w.hub.appendLog("news", clankmail.Sign(other, msgPayload(other, cid, "s", "https://spam.example/"+strconv.Itoa(i)+"\n"+big, testNow.UnixMilli())))
	}
	os.Remove(w.statePath())
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			var s runtime.MemStats
			runtime.ReadMemStats(&s)
			if s.HeapAlloc > peak.Load() {
				peak.Store(s.HeapAlloc)
			}
		}
	}()
	w.now = w.now.Add(time.Minute)
	res := w.run()
	close(stop)
	wg.Wait()
	if res.ExitCode() != 0 {
		t.Fatal(res.Errors)
	}
	growth := int64(peak.Load()) - int64(base)
	t.Logf("heap growth during the rebuild of a %d MB log: %d MB", 1000*44/1000, growth>>20)
	if growth > 24<<20 {
		t.Fatalf("heap grew %d MB rebuilding from the log, want under 24 MB (verify page by page)", growth>>20)
	}
}

// ---- 🟡5: a state write that fails after a submit ----

func TestY5StateWriteFailsAfterSubmit(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A"))
	w.config(5, feed("/a"))
	w.run() // state on disk
	oneItem(w, "1", "https://x.example/1", 1)
	// The state directory becomes unwritable while the post is in flight.
	sdir := filepath.Dir(w.statePath())
	w.hub.setOnStore(func(*clankmail.Object) { os.Chmod(sdir, 0o500) })
	defer os.Chmod(sdir, 0o700)
	w.now = w.now.Add(time.Minute)
	if res := w.run(); res.ExitCode() == 0 {
		t.Error("a failed state write must fail the run")
	}
	w.hub.setOnStore(nil)
	os.Chmod(sdir, 0o700)
	w.now = w.now.Add(time.Hour)
	w.run()
	if got := w.hub.firstLines("news"); !reflect.DeepEqual(got, []string{"https://x.example/1"}) {
		t.Fatalf("log is %v, want the item once", got)
	}
}

// ---- 🟢: a bare 429 stops submissions too ----

func TestG1Plain429Stops(t *testing.T) {
	w := newWorld(t)
	w.feeds.set("/a", atomFeedXML("A",
		entry{id: "1", link: "https://x.example/1", title: "1", date: ago(2)},
		entry{id: "2", link: "https://x.example/2", title: "2", date: ago(1)},
	))
	w.config(5, feed("/a"))
	w.cfg.Channels = append(w.cfg.Channels, Channel{Slug: "second", Title: "Second", MaxPerRun: 5, MaxAgeHours: 48,
		Feeds: []FeedConfig{{URL: w.feeds.url("/a")}}})
	w.hub.plain429After = 2 // genesis + one post
	res := w.run()
	if res.ExitCode() == 0 || !res.RateLimited {
		t.Fatalf("a bare 429 must stop the run as E_RATE does (exit %d, rate limited %v)", res.ExitCode(), res.RateLimited)
	}
	if w.hub.postSubmits != 2 {
		t.Errorf("%d post submissions, want 2 (the accepted one and the refused one)", w.hub.postSubmits)
	}
}
