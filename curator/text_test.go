package curator

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStripHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<p>Hello&nbsp;<b>world</b></p>`, "Hello world"},
		{`a < b and c > d`, "a < b and c > d"}, // a bare '<' is text
		{`<script>alert("x")</script>kept<style>p{}</style> too`, "kept too"},
		{`<SCRIPT>bad()</SCRIPT>ok`, "ok"},
		{`x<!-- hidden <b>y</b> -->z`, "x z"},
		{`&lt;script&gt;alert(1)&lt;/script&gt;`, "<script>alert(1)</script>"}, // decoded entities are inert text
		{"line1<br>line2\n\n\t line3", "line1 line2 line3"},
		{`AT&amp;T &#39;q&#39; &#x263A;`, "AT&T 'q' ☺"},
		{"bidi‮evil⁦x‏y", "bidievilxy"},
		{"ctrl\x00\x07char\x1b", "ctrlchar"},
		{"unterminated <a href=", "unterminated"},
		{"<script>never closed", ""},
		{"bad utf8 \xff\xfe end", "bad utf8 � end"},
	}
	for _, c := range cases {
		if got := StripHTML(c.in); got != c.want {
			t.Errorf("StripHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	// "é" is 2 bytes, "☺" is 3, "😀" is 4. Every cut point must stay valid.
	s := strings.Repeat("é☺😀a", 50)
	for max := 0; max <= len(s)+1; max++ {
		got := Truncate(s, max)
		if len(got) > max {
			t.Fatalf("Truncate(_, %d) is %d bytes", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(_, %d) = %q is not valid UTF-8", max, got)
		}
		if max >= len(s) && got != s {
			t.Fatalf("Truncate(_, %d) changed a string that fits", max)
		}
		if max < len(s) && max >= 3 && !strings.HasSuffix(got, "…") {
			t.Fatalf("Truncate(_, %d) = %q lacks the ellipsis", max, got)
		}
	}
	if got := Truncate("😀😀", 5); got != "…" {
		t.Errorf("Truncate(two 4-byte runes, 5) = %q, want just the ellipsis", got)
	}
	if got := Truncate("abcdef", 5); got != "ab…" {
		t.Errorf("Truncate(abcdef, 5) = %q, want ab…", got)
	}
}

func TestNormalizeLink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://Blog.Example/Post/1?utm_source=hn&id=7&UTM_Medium=rss#frag", "https://blog.example/Post/1?id=7"},
		{"HTTP://x.example/a?utm_campaign=z", "http://x.example/a"},
		{"https://x.example/a?b=2&a=1", "https://x.example/a?b=2&a=1"}, // order kept
		{"https://x.example/a#only-fragment", "https://x.example/a"},
		{"  https://x.example/trim  ", "https://x.example/trim"},
		{"javascript:alert(1)", ""},
		{"ftp://x.example/f", ""},
		{"/relative/path", ""},
		{"https://user:pw@x.example/", ""},
		{"https://x.example/has space", ""},
		{"", ""},
		{"https://x.example/" + strings.Repeat("a", 3000), ""},
	}
	for _, c := range cases {
		got := NormalizeLink(c.in)
		if got != c.want {
			t.Errorf("NormalizeLink(%q) = %q, want %q", c.in, got, c.want)
		}
		// The state rebuild re-normalises a posted link: it must be a fixed point.
		if got != "" && NormalizeLink(got) != got {
			t.Errorf("NormalizeLink not idempotent on %q: %q", got, NormalizeLink(got))
		}
	}
}

func TestMatchesKeyword(t *testing.T) {
	cases := []struct {
		text string
		kw   []string
		want bool
	}{
		{"LLM Agents in the wild", []string{"agent"}, true},
		{"Agentic workflows", []string{"agent"}, true},
		{"chemical reagents", []string{"agent"}, false},
		{"a reagent, then an agent", []string{"agent"}, true}, // second occurrence counts
		{"Tool use at scale", []string{"tool use"}, true},
		{"nothing here", []string{"agent", "tool"}, false},
		{"anything", []string{""}, false},
		{"Über-Agent", []string{"agent"}, true}, // '-' is a boundary
		{"v0.40.13-rc1", []string{"-rc"}, true}, // punctuation-led keyword: no boundary rule
		{"v0.40.13", []string{"-rc"}, false},
	}
	for _, c := range cases {
		if got := MatchesKeyword(c.text, c.kw); got != c.want {
			t.Errorf("MatchesKeyword(%q, %q) = %v, want %v", c.text, c.kw, got, c.want)
		}
	}
}
