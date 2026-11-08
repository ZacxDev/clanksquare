package editor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// StoryFormat is the first line of every story post body.
const StoryFormat = "clanksquare-story/1"

const (
	maxSubject   = 200
	maxExcerpt   = 500
	maxBodyItems = 16
	maxBodyLinks = 5
	maxVia       = 200
)

// The clanksquare-story/1 body: a machine-readable header, one "key: value"
// per line, ended by a blank line, then human-readable lines.
//
//	clanksquare-story/1
//	story: s_<20 chars>
//	url: <canonical URL>
//	sources: <number of distinct sources>
//	items: <m_ id> <m_ id> …          (the raw posts, at most 16)
//	score: post=0.78 digest=0.20 drop=0.02
//
//	<title>
//	<link>                            (each distinct raw link, at most 5)
//	via <source>, <source>
//
//	<excerpt, at most 500 bytes>      (omitted when empty)
//
// Header keys appear in exactly this order. Readers MUST ignore header keys
// they do not know, so later versions can add keys before the blank line.
// The excerpt is the first raw item's own text; no model wrote any of it.

// StoryHeader is the parsed header of a story body.
type StoryHeader struct {
	Story   string
	URL     string
	Sources int
	Items   []string
	Score   map[string]float64
}

// oneLine makes untrusted text safe for a single body line.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || r >= '‪' && r <= '‮' || r >= '⁦' && r <= '⁩' {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// truncate cuts s to at most n bytes, ending in "…" if cut, never inside a
// UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	return strings.TrimRight(clipText(s, n-len(ell)), " ") + ell
}

// storySources are the distinct source names of a story, in item order.
func storySources(st *Story) []string {
	var out []string
	for _, it := range st.Items {
		v := oneLine(it.Via)
		if v == "" {
			v = it.Channel
		}
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// storyURL is the story's canonical URL: its first item's, or its first
// raw link when that has none.
func storyURL(st *Story) string {
	for _, it := range st.Items {
		if it.Canon != "" {
			return it.Canon
		}
	}
	return oneLine(st.Items[0].Link)
}

// FormatStory renders a story post. d supplies the score line.
func FormatStory(st *Story, d *StoryDecision) (subject, body string) {
	first := st.Items[0]
	title := oneLine(first.Title)
	subject = truncate(title, maxSubject)
	if subject == "" {
		subject = truncate(storyURL(st), maxSubject)
	}
	var ids []string
	for i, it := range st.Items {
		if i == maxBodyItems {
			break
		}
		ids = append(ids, it.ID)
	}
	var links []string
	for _, it := range st.Items {
		l := oneLine(it.Link)
		if l != "" && !contains(links, l) && len(links) < maxBodyLinks {
			links = append(links, l)
		}
	}
	sources := storySources(st)
	var b strings.Builder
	b.WriteString(StoryFormat + "\n")
	fmt.Fprintf(&b, "story: %s\n", st.ID)
	fmt.Fprintf(&b, "url: %s\n", storyURL(st))
	fmt.Fprintf(&b, "sources: %d\n", len(sources))
	fmt.Fprintf(&b, "items: %s\n", strings.Join(ids, " "))
	fmt.Fprintf(&b, "score: post=%.2f digest=%.2f drop=%.2f\n", d.Probs["post"], d.Probs["digest"], d.Probs["drop"])
	b.WriteString("\n")
	if title != "" {
		b.WriteString(truncate(title, maxSubject) + "\n")
	}
	for _, l := range links {
		b.WriteString(l + "\n")
	}
	b.WriteString("via " + truncate(strings.Join(sources, ", "), maxVia))
	if ex := truncate(oneLine(first.Excerpt), maxExcerpt); ex != "" {
		b.WriteString("\n\n" + ex)
	}
	return subject, b.String()
}

// ParseStory parses a story body's header.
func ParseStory(body string) (*StoryHeader, error) {
	head, _, ok := strings.Cut(body, "\n\n")
	if !ok {
		return nil, errors.New("story: no blank line after the header")
	}
	lines := strings.Split(head, "\n")
	if lines[0] != StoryFormat {
		return nil, fmt.Errorf("story: first line is not %s", StoryFormat)
	}
	h := &StoryHeader{Score: map[string]float64{}}
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ": ")
		if !ok {
			return nil, fmt.Errorf("story: bad header line %q", l)
		}
		switch k {
		case "story":
			h.Story = v
		case "url":
			h.URL = v
		case "sources":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("story: bad sources %q", v)
			}
			h.Sources = n
		case "items":
			h.Items = strings.Fields(v)
		case "score":
			for _, f := range strings.Fields(v) {
				lk, lv, ok := strings.Cut(f, "=")
				p, err := strconv.ParseFloat(lv, 64)
				if !ok || err != nil {
					return nil, fmt.Errorf("story: bad score %q", f)
				}
				h.Score[lk] = p
			}
		}
	}
	if !strings.HasPrefix(h.Story, "s_") {
		return nil, errors.New("story: no story id")
	}
	return h, nil
}
