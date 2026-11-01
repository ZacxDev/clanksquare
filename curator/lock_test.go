//go:build unix

package curator

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestY4OverlappingRunIsRefused(t *testing.T) {
	// Another run holds the lock: this one posts nothing and fails.
	w := newWorld(t)
	oneItem(w, "1", "https://x.example/1", 1)
	w.config(5, feed("/a"))
	f, err := os.OpenFile(w.statePath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	res := w.run()
	if res.ExitCode() == 0 || len(w.hub.submits) != 0 {
		t.Fatalf("a run overlapping a locked one: exit %d, submits %v; want failure and nothing submitted", res.ExitCode(), w.hub.submits)
	}
	if !strings.Contains(w.out.String(), "another curator run holds") {
		t.Error("the lock refusal should say why")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if res := w.run(); res.ExitCode() != 0 || len(w.hub.posts("news")) != 1 {
		t.Fatalf("after the lock is released: exit %d, %d posts", res.ExitCode(), len(w.hub.posts("news")))
	}
}
