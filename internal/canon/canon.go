// Package canon maps a link to the key the editor groups stories by: two links
// that name the same thing (an arXiv abstract and its PDF, a tweet on
// twitter.com and on x.com, a URL with and without utm_ parameters) get the
// same key. It is deterministic and has no network access.
//
// The curator's own NormalizeLink (curator/text.go) is a different, narrower
// function on purpose: it decides what the curator posts and must stay stable
// for its seen-set rebuild. Changing it would repost history; this package is
// free to grow.
package canon

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// maxLink bounds the input; longer links are not keyed.
const maxLink = 2048

// trackingParams are query keys dropped from every URL. utm_* is matched by
// prefix in isTracking.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "yclid": true,
	"mc_cid": true, "mc_eid": true, "_hsenc": true, "_hsmi": true, "igshid": true,
	"ref": true, "ref_src": true, "ref_url": true, "spm": true, "share_source": true,
	"__twitter_impression": true,
}

func isTracking(host, key string) bool {
	k := strings.ToLower(key)
	if strings.HasPrefix(k, "utm_") || trackingParams[k] {
		return true
	}
	// "si" is a share id on YouTube and Spotify; elsewhere it may be real.
	if k == "si" && (host == "youtube.com" || host == "open.spotify.com") {
		return true
	}
	return false
}

// hostAliases rewrite a host to the one its canonical form uses.
var hostAliases = map[string]string{
	"twitter.com":        "x.com",
	"mobile.twitter.com": "x.com",
	"mobile.x.com":       "x.com",
	"export.arxiv.org":   "arxiv.org",
	"old.reddit.com":     "reddit.com",
	"np.reddit.com":      "reddit.com",
	"m.youtube.com":      "youtube.com",
	"m.wikipedia.org":    "wikipedia.org",
}

// arXiv identifiers: new style 2610.40556 (4 or 5 digits after the dot),
// old style cs/0112017 or math.GT/0309136, each with an optional vN.
var (
	arxivNew = regexp.MustCompile(`^(\d{4}\.\d{4,5})(v\d+)?$`)
	arxivOld = regexp.MustCompile(`^([a-z-]+(?:\.[A-Za-z]{2})?/\d{7})(v\d+)?$`)
)

// ArxivID returns the version-less arXiv id a link names, or "". It accepts
// the abs, pdf and html pages on arxiv.org (www. and export. too), with or
// without a ".pdf" suffix and a version, and Hugging Face paper pages.
func ArxivID(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := normHost(u.Hostname())
	p := strings.Trim(u.EscapedPath(), "/")
	var rest string
	switch host {
	case "arxiv.org":
		for _, pre := range []string{"abs/", "pdf/", "html/"} {
			if strings.HasPrefix(p, pre) {
				rest = strings.TrimPrefix(p, pre)
				break
			}
		}
	case "huggingface.co", "hf.co":
		rest = strings.TrimPrefix(p, "papers/")
		if rest == p {
			return ""
		}
	default:
		return ""
	}
	rest = strings.TrimSuffix(rest, ".pdf")
	if m := arxivNew.FindStringSubmatch(rest); m != nil {
		return m[1]
	}
	if m := arxivOld.FindStringSubmatch(rest); m != nil {
		return m[1]
	}
	return ""
}

// normHost lowercases a host, drops a leading "www." and applies hostAliases.
func normHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	h = strings.TrimPrefix(h, "www.")
	if a, ok := hostAliases[h]; ok {
		return a
	}
	return h
}

// Key returns the canonical key of a link, or "" for anything that is not an
// absolute http(s) URL. Rules, in order:
//
//   - arXiv abs/pdf/html pages and Hugging Face paper pages become
//     https://arxiv.org/abs/<id> with no version;
//   - youtu.be/<id> becomes youtube.com/watch?v=<id>;
//   - the scheme becomes https, the host is lowercased, "www." and a default
//     port are dropped, and hostAliases apply (twitter.com -> x.com);
//   - the fragment and tracking parameters are dropped, the remaining query
//     parameters are sorted (all of them on x.com, where every one is
//     tracking);
//   - a trailing "/" is dropped from the path.
//
// Key is idempotent: Key(Key(x)) == Key(x).
func Key(raw string) string {
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
	if id := ArxivID(raw); id != "" {
		return "https://arxiv.org/abs/" + id
	}
	host := normHost(u.Hostname())
	if host == "" {
		return ""
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		host += ":" + port
	}
	path := u.EscapedPath()
	q := u.Query()
	if host == "youtu.be" {
		if v := strings.Trim(path, "/"); v != "" && !strings.Contains(v, "/") {
			host, path = "youtube.com", "/watch"
			q.Set("v", v)
		}
	}
	keep := url.Values{}
	if host != "x.com" {
		for k, vs := range q {
			if !isTracking(host, k) {
				keep[k] = vs
			}
		}
	}
	path = strings.TrimRight(path, "/")
	out := "https://" + host + path
	if len(keep) > 0 {
		keys := make([]string, 0, len(keep))
		for k := range keep {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			vs := append([]string(nil), keep[k]...)
			sort.Strings(vs)
			for _, v := range vs {
				parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
			}
		}
		out += "?" + strings.Join(parts, "&")
	}
	return out
}

// articleURL finds the target an aggregator item carries in its text:
// hnrss.org writes "Article URL: <url>" into the item description.
var articleURL = regexp.MustCompile(`(?i)\bArticle URL:\s*(https?://\S+)`)

// Target returns the link an item is ABOUT: for a Hacker News item page or a
// Lobsters story page whose text names the article ("Article URL: …"), that
// article; otherwise link itself. It returns the raw link; key it with Key.
func Target(link, text string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return link
	}
	host := normHost(u.Hostname())
	isAgg := host == "news.ycombinator.com" && u.Path == "/item" ||
		host == "lobste.rs" && strings.HasPrefix(u.Path, "/s/")
	if !isAgg {
		return link
	}
	if m := articleURL.FindStringSubmatch(text); m != nil {
		t := strings.TrimRight(m[1], ".,;)")
		if Key(t) != "" {
			return t
		}
	}
	return link
}
