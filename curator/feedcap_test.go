package curator

// 🟡1 (PR #9 round 1): decoding stops at the item cap.

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestParseStopsAtItemCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`)
	for i := 0; i < maxItemsPerFeed; i++ {
		fmt.Fprintf(&b, "<item><title>%d</title><link>https://x.example/%d</link></item>", i, i)
	}
	b.WriteString("<item><title>broken</channel>") // never reached
	f, err := ParseFeed([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseFeed read past the item cap: %v", err)
	}
	if len(f.Items) != maxItemsPerFeed {
		t.Fatalf("%d items, want %d", len(f.Items), maxItemsPerFeed)
	}
}

func TestParseHugeFeedBoundedAlloc(t *testing.T) {
	head := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`
	body := strings.Repeat("<item/>", (MaxFeedBytes-len(head)-30)/7)
	data := []byte(head + body + "</channel></rss>")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	f, err := ParseFeed(data)
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != maxItemsPerFeed {
		t.Fatalf("%d items", len(f.Items))
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 32<<20 {
		t.Fatalf("parsing a %d-byte feed allocated %d MB, want under 32 MB (decoding must stop at the cap)", len(data), alloc>>20)
	}
}
