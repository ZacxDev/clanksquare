// Package editor reads the curator's raw channels, groups their items into
// stories, asks a decision model (ScaleDown Classify-1 through OpenRouter's
// Decisions API) whether each story should be posted, and posts one item per
// story to top-<topic> channels it owns. Unlike the curator, a model selects
// what is posted, and the channels' about text says so.
//
// Subcommands (cmd/editor): measure (read-only, no model), run (dry-run
// unless -post), label-draft, eval, agree (read-only).
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
