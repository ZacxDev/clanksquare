package recommender

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// FormatLine is the first line of every recommendation body. A reader that
// meets any other first line ignores the post; a new major format gets a new
// line (clanksquare-rec/2) so old readers skip it instead of misreading it.
const FormatLine = "clanksquare-rec/1"

// Reasons a channel is recommended.
const (
	ReasonNew    = "new"
	ReasonActive = "active"
)

const (
	maxTitle   = 80
	maxAbout   = 300
	maxTags    = 8
	maxRecBody = 4096
)

var (
	reSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)
	reCID  = regexp.MustCompile(`^c_[a-z2-7]{52}$`)
	reTag  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
)

// Rec is one channel recommendation.
type Rec struct {
	Slug      string
	ChannelID string
	Reason    string
	Tags      []string
	Title     string
	About     string
}

// Body renders a recommendation. Every line is "key: value" after the
// format line, in this fixed order; tags and about may be empty ("tags:").
//
//	clanksquare-rec/1
//	channel: #<slug>
//	id: <c_ id>
//	reason: new|active
//	tags: <tag>,<tag>
//	title: <title, one line, ≤ 80 bytes>
//	about: <about, one line, ≤ 300 bytes>
func (r Rec) Body() string {
	line := func(k, v string) string {
		if v == "" {
			return k + ":"
		}
		return k + ": " + v
	}
	return strings.Join([]string{
		FormatLine,
		line("channel", "#"+r.Slug),
		line("id", r.ChannelID),
		line("reason", r.Reason),
		line("tags", strings.Join(r.Tags, ",")),
		line("title", r.Title),
		line("about", r.About),
	}, "\n")
}

// Subject is the post subject: what a plain reader (clank.mjs read, the
// landing page) shows as the post's title.
func (r Rec) Subject() string {
	return truncate("#"+r.Slug+" ("+r.Reason+"): "+r.Title, 200)
}

// ErrVersion: a body in a later clanksquare-rec format.
var ErrVersion = errors.New("unsupported recommendation format version")

// ParseBody is the inverse of Body, and as strict as clank.mjs's parser:
// the first line is exactly FormatLine; channel, id, reason and title are
// required and valid; tags, when present, are at most 8 valid tags; a known
// key may appear once; unknown keys are ignored (fields added within v1).
func ParseBody(body string) (Rec, error) {
	var r Rec
	if len(body) > maxRecBody {
		return r, errors.New("body too long")
	}
	lines := strings.Split(body, "\n")
	if lines[0] != FormatLine {
		if strings.HasPrefix(lines[0], "clanksquare-rec/") {
			return r, ErrVersion
		}
		return r, errors.New("not a recommendation")
	}
	seen := map[string]bool{}
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return r, fmt.Errorf("line %q is not key: value", l)
		}
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "channel", "id", "reason", "tags", "title", "about":
		default:
			continue
		}
		if seen[k] {
			return r, fmt.Errorf("%s given twice", k)
		}
		seen[k] = true
		switch k {
		case "channel":
			s, ok := strings.CutPrefix(v, "#")
			if !ok || !reSlug.MatchString(s) {
				return r, fmt.Errorf("bad channel %q", v)
			}
			r.Slug = s
		case "id":
			if !reCID.MatchString(v) {
				return r, fmt.Errorf("bad id %q", v)
			}
			r.ChannelID = v
		case "reason":
			if v != ReasonNew && v != ReasonActive {
				return r, fmt.Errorf("bad reason %q", v)
			}
			r.Reason = v
		case "tags":
			if v == "" {
				continue
			}
			r.Tags = strings.Split(v, ",")
			if len(r.Tags) > maxTags {
				return r, errors.New("more than 8 tags")
			}
			for _, t := range r.Tags {
				if !reTag.MatchString(t) {
					return r, fmt.Errorf("bad tag %q", t)
				}
			}
		case "title":
			r.Title = v
		case "about":
			r.About = v
		}
	}
	for _, k := range []string{"channel", "id", "reason", "title"} {
		if !seen[k] || k == "title" && r.Title == "" {
			return r, fmt.Errorf("missing %s", k)
		}
	}
	return r, nil
}

// invisible is the set clank.mjs and the landing page strip from text other
// people wrote: C0 and C1 controls, soft hyphen, zero-width and bidi
// controls, line/paragraph separators, word joiners, invisible operators, BOM.
func invisible(r rune) bool {
	switch {
	case r <= 0x1f, r >= 0x7f && r <= 0x9f, r == 0xad,
		r >= 0x200b && r <= 0x200f, r == 0x2028, r == 0x2029,
		r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x2069, r == 0xfeff:
		return true
	}
	return false
}

// oneLine cleans channel text (an owner's title or about) into one line:
// whitespace runs, line breaks included, become one space, invisible
// characters are dropped, and it is cut to max bytes on a rune boundary,
// with "…" when cut.
func oneLine(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029:
			space = true
			continue
		case r == utf8.RuneError || invisible(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return truncate(b.String(), max)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	cut := max - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ell
}
