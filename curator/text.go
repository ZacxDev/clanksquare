package curator

import (
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Feed text is untrusted. Everything here turns it into bounded, single-line,
// valid UTF-8 plain text, and does it the same way every time.

// maxRawField bounds how much of one feed field is processed at all, so a
// hostile feed cannot make stripping expensive.
const maxRawField = 64 << 10

// skipContent lists elements whose content is never text a reader should see.
var skipContent = map[string]bool{"script": true, "style": true, "head": true}

// StripHTML returns the plain text of an HTML fragment: comments, tags and the
// content of script/style removed, entities decoded, control and bidi
// characters dropped, whitespace collapsed to single spaces. A '<' that does
// not start a tag (as in "a < b") is kept as text.
func StripHTML(s string) string {
	s = clip(s, maxRawField)
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '<' || !tagStart(s[i+1:]) {
			b.WriteByte(c)
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break // unterminated comment: drop the rest
			}
			i += 4 + end + 3
			b.WriteByte(' ')
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break // unterminated tag: drop the rest
		}
		inside := s[i+1 : i+end]
		name := tagName(inside)
		i += end + 1
		b.WriteByte(' ')
		if inside[0] != '/' && skipContent[name] {
			// Skip to the matching close tag (case-insensitive); if there is
			// none, the rest of the field is that element's content.
			closeAt := indexFold(s[i:], "</"+name)
			if closeAt < 0 {
				break
			}
			i += closeAt
		}
	}
	return PlainText(html.UnescapeString(b.String()))
}

// tagStart reports whether the text after a '<' begins a tag, end tag,
// comment, doctype or processing instruction.
func tagStart(rest string) bool {
	if rest == "" {
		return false
	}
	c := rest[0]
	return c == '/' || c == '!' || c == '?' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tagName is the lowercased element name of a tag's inside ("p class=x" -> "p").
func tagName(inside string) string {
	inside = strings.TrimPrefix(inside, "/")
	end := strings.IndexFunc(inside, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	if end >= 0 {
		inside = inside[:end]
	}
	return strings.ToLower(inside)
}

// indexFold is strings.Index with ASCII case folding. sub must be ASCII (a
// close tag). It returns an offset into s itself: lowering s first would
// not, because strings.ToLower changes byte lengths (the Kelvin sign, İ,
// invalid UTF-8) and its offsets would then point into the wrong place.
func indexFold(s, sub string) int {
	n := len(sub)
outer:
	for i := 0; i+n <= len(s); i++ {
		for j := 0; j < n; j++ {
			if asciiLower(s[i+j]) != asciiLower(sub[j]) {
				continue outer
			}
		}
		return i
	}
	return -1
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// PlainText normalises text that is already plain: invalid UTF-8 replaced,
// control and bidirectional-override characters dropped, every run of
// whitespace collapsed to one space, trimmed.
func PlainText(s string) string {
	s = strings.ToValidUTF8(clip(s, maxRawField), "\uFFFD")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r) || isBidiControl(r) || r == 0xFEFF:
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// isBidiControl reports the embedding, override and isolate controls, which
// can make displayed text read differently from its bytes.
func isBidiControl(r rune) bool {
	return r >= '\u202A' && r <= '\u202E' || r >= '\u2066' && r <= '\u2069' || r == '\u200E' || r == '\u200F'
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Truncate returns s if it fits in max bytes, else the longest prefix that,
// with "…" appended, fits in max bytes. It never splits a UTF-8 sequence.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	if max < len(ell) {
		return clip(s, max)
	}
	return strings.TrimRight(clip(s, max-len(ell)), " ") + ell
}

// NormalizeLink returns the dedupe form of a feed link: an absolute http(s)
// URL with the scheme and host lowercased, the fragment dropped and every
// utm_* query parameter removed (the others keep their order). It returns ""
// for anything that is not an absolute http(s) URL. NormalizeLink is
// idempotent, which the state rebuild depends on.
func NormalizeLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxLink || strings.ContainsAny(raw, " \t\r\n") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	if u.RawQuery != "" {
		var kept []string
		for _, p := range strings.Split(u.RawQuery, "&") {
			if p == "" {
				continue
			}
			k := p
			if i := strings.IndexByte(p, '='); i >= 0 {
				k = p[:i]
			}
			if strings.HasPrefix(strings.ToLower(k), "utm_") {
				continue
			}
			kept = append(kept, p)
		}
		u.RawQuery = strings.Join(kept, "&")
	}
	u.ForceQuery = false
	out := u.String()
	if len(out) > maxLink || !utf8.ValidString(out) {
		return ""
	}
	return out
}

// maxLink bounds a link (and a guid used in its place) in bytes.
const maxLink = 2048

// MatchesKeyword reports whether any keyword occurs in text, case-insensitively,
// starting at a word boundary: "agent" matches "agents" and "Agentic" but not
// "reagent". A keyword starting with punctuation ("-rc") matches anywhere.
// Empty keywords never match.
func MatchesKeyword(text string, keywords []string) bool {
	low := strings.ToLower(text)
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		for from := 0; ; {
			i := strings.Index(low[from:], k)
			if i < 0 {
				break
			}
			at := from + i
			// The boundary rule applies only to a keyword that starts with a
			// letter or digit: "-rc" must match "v1.2-rc1".
			if at == 0 || !isWordRune(lastRune(low[:at])) || !isWordRune(firstRune(k)) {
				return true
			}
			from = at + 1
		}
	}
	return false
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// hostOf returns the lowercased host of an http(s) URL, or "".
func hostOf(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
