package canon

import "testing"

func TestKey(t *testing.T) {
	cases := []struct{ in, want string }{
		// arXiv: abs, pdf, html, versions and hosts collapse to one key.
		{"https://arxiv.org/abs/2610.40556v1", "https://arxiv.org/abs/2610.40556"},
		{"https://arxiv.org/abs/2610.40556", "https://arxiv.org/abs/2610.40556"},
		{"http://arxiv.org/pdf/2610.40556v2", "https://arxiv.org/abs/2610.40556"},
		{"https://arxiv.org/pdf/2610.40556v3.pdf", "https://arxiv.org/abs/2610.40556"},
		{"https://arxiv.org/html/2610.40556v1/", "https://arxiv.org/abs/2610.40556"},
		{"https://export.arxiv.org/abs/2610.40556v1", "https://arxiv.org/abs/2610.40556"},
		{"https://www.arxiv.org/abs/2610.40556?context=cs.AI", "https://arxiv.org/abs/2610.40556"},
		{"https://huggingface.co/papers/2610.40556", "https://arxiv.org/abs/2610.40556"},
		{"https://arxiv.org/abs/cs/0112017v1", "https://arxiv.org/abs/cs/0112017"},
		{"https://arxiv.org/abs/math.GT/0309136", "https://arxiv.org/abs/math.GT/0309136"},
		{"https://arxiv.org/abs/2401.0001", "https://arxiv.org/abs/2401.0001"},
		// An arXiv page that is not a paper keeps its path.
		{"https://arxiv.org/list/cs.AI/recent", "https://arxiv.org/list/cs.AI/recent"},
		// Tracking parameters, fragment, host, trailing slash, query order.
		{"HTTP://WWW.Example.COM:443/a/b/?utm_source=x&b=2&a=1&fbclid=z#frag", "https://example.com/a/b?a=1&b=2"},
		{"https://example.com/", "https://example.com"},
		{"https://example.com:8080/x", "https://example.com:8080/x"},
		{"https://example.com/x?ref=hn", "https://example.com/x"},
		{"https://example.com/x?si=keep", "https://example.com/x?si=keep"},
		{"https://youtu.be/abc123?si=share", "https://youtube.com/watch?v=abc123"},
		{"https://www.youtube.com/watch?v=abc123&si=share", "https://youtube.com/watch?v=abc123"},
		// twitter.com becomes x.com and loses every query parameter.
		{"https://twitter.com/user/status/123?s=20&t=abc", "https://x.com/user/status/123"},
		{"https://mobile.twitter.com/user/status/123", "https://x.com/user/status/123"},
		{"https://old.reddit.com/r/selfhosted/comments/1xl9bgr/x/", "https://reddit.com/r/selfhosted/comments/1xl9bgr/x"},
		// Not keyable.
		{"", ""},
		{"ftp://example.com/x", ""},
		{"example.com/x", ""},
		{"https://user:pw@example.com/", ""},
		{"https://example.com/a b", ""},
	}
	for _, c := range cases {
		if got := Key(c.in); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.want != "" {
			if again := Key(c.want); again != c.want {
				t.Errorf("Key not idempotent: Key(%q) = %q", c.want, again)
			}
		}
	}
}

// The arXiv abs/pdf pair must collapse; this is the guard the mutation test
// for the arXiv rule targets.
func TestArxivAbsPdfCollapse(t *testing.T) {
	a, b := Key("https://arxiv.org/abs/2610.40565v1"), Key("https://arxiv.org/pdf/2610.40565v2")
	if a != b || a != "https://arxiv.org/abs/2610.40565" {
		t.Fatalf("arxiv abs/pdf did not collapse: %q vs %q", a, b)
	}
}

func TestTarget(t *testing.T) {
	cases := []struct{ link, text, want string }{
		{"https://news.ycombinator.com/item?id=1", "Article URL: https://example.com/post Comments URL: https://news.ycombinator.com/item?id=1 Points: 5", "https://example.com/post"},
		{"https://lobste.rs/s/abc123/title", "article url: https://example.com/p.", "https://example.com/p"},
		{"https://news.ycombinator.com/item?id=1", "Ask HN: no article here", "https://news.ycombinator.com/item?id=1"},
		// Only aggregator pages are rewritten: a normal link keeps its own
		// target even if its text names another URL.
		{"https://example.com/a", "Article URL: https://evil.example/b", "https://example.com/a"},
		{"https://news.ycombinator.com/news", "Article URL: https://example.com/b", "https://news.ycombinator.com/news"},
	}
	for _, c := range cases {
		if got := Target(c.link, c.text); got != c.want {
			t.Errorf("Target(%q, %q) = %q, want %q", c.link, c.text, got, c.want)
		}
	}
}
