// Package editor reads the curator's raw channels, groups their items into
// stories by canonical URL, asks a decision model (Perplexity Decider through
// OpenRouter's Decisions API, by default) for each story's P(post), and posts
// each day's top stories, one item per story, to top-<topic> channels it owns.
// Unlike the curator, a model selects what is posted, and the channels' about
// text says so.
//
// Subcommands (cmd/editor): measure (read-only, no model), run (dry-run
// unless -post), truth (read-only), eval.
package editor

import (
	"net/http"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// Version is reported in the User-Agent.
const Version = "0.1.0"

// UserAgent is sent on every hub and lookup request.
const UserAgent = "clanksquare-editor/" + Version + " (+https://github.com/ZacxDev/clanksquare)"

// NewHub returns a hub client for base.
func NewHub(base string, c *http.Client) *hubkit.Client {
	if c == nil {
		c = &http.Client{}
	}
	return &hubkit.Client{Base: base, HTTP: c, UserAgent: UserAgent}
}
