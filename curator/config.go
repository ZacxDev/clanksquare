package curator

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

const (
	defaultMaxAgeHours = 48
	defaultMaxPerRun   = 5
	maxMaxPerRun       = 50
	// maxMaxAgeHours is a sanity bound on max_age_hours (one year). It is
	// not tied to how long posted links are remembered: they never expire.
	maxMaxAgeHours = 365 * 24
)

// Config is the curator's JSON configuration.
type Config struct {
	// Hub is the hub's DNS name, signed into handle objects (SPEC.md §6.1).
	Hub string `json:"hub"`
	// HubURL is where the hub is reached, e.g. https://clankmail.zacx.dev or
	// an internal address. It may differ from Hub.
	HubURL string `json:"hub_url"`
	// Handle is an optional handle name to claim for the curator's key.
	Handle string `json:"handle,omitempty"`
	// MaxAgeHours: items older than this are skipped (default 48).
	MaxAgeHours int `json:"max_age_hours,omitempty"`
	// XSearchMonthlyCapUSD is the hard cap on the ESTIMATED xAI spend of all
	// xsearch feeds in one calendar month (UTC). Default 60. See xsearch.go.
	XSearchMonthlyCapUSD float64   `json:"xsearch_monthly_cap_usd,omitempty"`
	Channels             []Channel `json:"channels"`
}

// Channel is one topic channel and the feeds that fill it.
type Channel struct {
	Slug  string   `json:"slug"`
	Title string   `json:"title"`
	About string   `json:"about,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	// MaxPerRun caps posts per run (default 5).
	MaxPerRun int `json:"max_per_run,omitempty"`
	// MaxAgeHours overrides the top-level value for this channel.
	MaxAgeHours int          `json:"max_age_hours,omitempty"`
	Feeds       []FeedConfig `json:"feeds"`
}

// Source kinds. Every kind yields Items; dedupe, filters, caps and posting
// are shared (selectItems, formatPost).
const (
	KindRSS     = "rss"     // RSS 2.0, RSS 1.0 (RDF) or Atom at URL; the default
	KindXSearch = "xsearch" // X posts found by xAI's x_search tool (xsearch.go)
	KindDiscord = "discord" // one Discord channel, read by a bot (discord.go)
)

// FeedConfig is one source and its keyword filters.
type FeedConfig struct {
	// Kind is rss (default), xsearch or discord.
	Kind string `json:"kind,omitempty"`
	// URL is the feed URL (rss only).
	URL string `json:"url,omitempty"`
	// Name, when set, replaces the feed's own title in the "via" line.
	Name    string   `json:"name,omitempty"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	// Disabled sources are validated but never fetched.
	Disabled bool `json:"disabled,omitempty"`
	// MaxPerRun, when set, caps this source's posts per run, inside the
	// channel's own cap (for a source that publishes far more often than the
	// others in its channel).
	MaxPerRun int `json:"max_per_run,omitempty"`
	// TitleFromSummary (rss): an item with no title takes the start of its
	// summary as its title (Bluesky profile feeds have no titles).
	TitleFromSummary bool `json:"title_from_summary,omitempty"`

	// xsearch: allowed_x_handles (≤ 20, no @) and/or query, the lookback
	// window, the minimum hours between calls, the model and its max turns.
	AllowedXHandles []string `json:"allowed_x_handles,omitempty"`
	Query           string   `json:"query,omitempty"`
	LookbackHours   int      `json:"lookback_hours,omitempty"`
	IntervalHours   int      `json:"interval_hours,omitempty"`
	Model           string   `json:"model,omitempty"`
	MaxTurns        int      `json:"max_turns,omitempty"`

	// discord: the channel to read and its server (for message permalinks).
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
}

// label names a source in logs. For rss it is the URL, as before.
func (f FeedConfig) label() string {
	switch f.Kind {
	case KindXSearch:
		return "xsearch:" + f.sourceID()
	case KindDiscord:
		return "discord:" + f.ChannelID
	}
	return f.URL
}

// LoadConfig reads, defaults and validates a config file. Unknown fields are
// errors, so a typo cannot silently disable a filter.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// Validate fills defaults and checks every field. Manifests and the handle are
// checked by signing them with a throwaway key and running the reference §4
// pipeline, so the curator never disagrees with the spec about what is valid.
func (c *Config) Validate() error {
	if c.HubURL == "" {
		return errors.New("hub_url is required")
	}
	if u, err := url.Parse(c.HubURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("hub_url %q: want an http(s) URL", c.HubURL)
	}
	c.HubURL = strings.TrimRight(c.HubURL, "/")
	if c.MaxAgeHours == 0 {
		c.MaxAgeHours = defaultMaxAgeHours
	}
	if c.MaxAgeHours < 1 || c.MaxAgeHours > maxMaxAgeHours {
		return fmt.Errorf("max_age_hours must be 1–%d", maxMaxAgeHours)
	}
	if c.XSearchMonthlyCapUSD == 0 {
		c.XSearchMonthlyCapUSD = defaultXSearchCapUSD
	}
	if !(c.XSearchMonthlyCapUSD > 0 && c.XSearchMonthlyCapUSD <= maxXSearchCapUSD) {
		return fmt.Errorf("xsearch_monthly_cap_usd must be > 0 and ≤ %d", maxXSearchCapUSD)
	}
	_, probe, _ := ed25519.GenerateKey(nil)
	if c.Hub == "" {
		return errors.New("hub (the hub's DNS name) is required")
	}
	// hub is only signed into the handle, but a bad one is a bad config, so
	// it is checked with a stand-in name when no handle is configured.
	name := c.Handle
	if name == "" {
		name = "probe"
	}
	if _, err := clankmail.Verify(clankmail.Sign(probe, handlePayload(probe, name, c.Hub, 1))); err != nil {
		return fmt.Errorf("handle/hub: %v", err)
	}
	if len(c.Channels) == 0 {
		return errors.New("no channels")
	}
	slugs := map[string]bool{}
	for i := range c.Channels {
		ch := &c.Channels[i]
		if slugs[ch.Slug] {
			return fmt.Errorf("channel %q listed twice", ch.Slug)
		}
		slugs[ch.Slug] = true
		if _, err := clankmail.Verify(clankmail.Sign(probe, ch.manifest(probe, "", 1))); err != nil {
			return fmt.Errorf("channel %q: %v", ch.Slug, err)
		}
		if ch.MaxPerRun == 0 {
			ch.MaxPerRun = defaultMaxPerRun
		}
		if ch.MaxPerRun < 1 || ch.MaxPerRun > maxMaxPerRun {
			return fmt.Errorf("channel %q: max_per_run must be 1–%d", ch.Slug, maxMaxPerRun)
		}
		if ch.MaxAgeHours == 0 {
			ch.MaxAgeHours = c.MaxAgeHours
		}
		if ch.MaxAgeHours < 1 || ch.MaxAgeHours > maxMaxAgeHours {
			return fmt.Errorf("channel %q: max_age_hours must be 1–%d", ch.Slug, maxMaxAgeHours)
		}
		if len(ch.Feeds) == 0 {
			return fmt.Errorf("channel %q: no feeds", ch.Slug)
		}
		for j := range ch.Feeds {
			if err := ch.Feeds[j].validate(); err != nil {
				return fmt.Errorf("channel %q: feed %d: %v", ch.Slug, j+1, err)
			}
		}
	}
	return nil
}

const (
	defaultLookbackHours = 24
	defaultIntervalHours = 6
	maxLookbackHours     = 7 * 24
	defaultXModel        = "grok-4.7"
	defaultMaxTurns      = 2
	maxMaxTurns          = 5
	maxXHandles          = 20 // the x_search tool's own limit
	maxQuery             = 500
)

var (
	xHandleRe   = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)
	snowflakeRe = regexp.MustCompile(`^[0-9]{15,20}$`)
)

// validate fills a source's defaults and checks it. Fields that belong to
// another kind are errors, so a misplaced field cannot silently do nothing.
func (f *FeedConfig) validate() error {
	if f.Kind == "" {
		f.Kind = KindRSS
	}
	if f.MaxPerRun < 0 || f.MaxPerRun > maxMaxPerRun {
		return fmt.Errorf("max_per_run must be 0–%d", maxMaxPerRun)
	}
	xs := len(f.AllowedXHandles) > 0 || f.Query != "" || f.LookbackHours != 0 || f.IntervalHours != 0 || f.Model != "" || f.MaxTurns != 0
	dc := f.ChannelID != "" || f.GuildID != ""
	switch f.Kind {
	case KindRSS:
		if xs || dc {
			return errors.New("xsearch/discord fields on an rss feed")
		}
		if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("feed url %q: want an http(s) URL", f.URL)
		}
	case KindXSearch:
		if f.URL != "" || dc || f.TitleFromSummary {
			return errors.New("url/discord/title_from_summary fields on an xsearch source")
		}
		if len(f.AllowedXHandles) == 0 && strings.TrimSpace(f.Query) == "" {
			return errors.New("xsearch needs allowed_x_handles and/or query")
		}
		if len(f.AllowedXHandles) > maxXHandles {
			return fmt.Errorf("allowed_x_handles: at most %d", maxXHandles)
		}
		for _, h := range f.AllowedXHandles {
			if !xHandleRe.MatchString(h) {
				return fmt.Errorf("allowed_x_handles: %q is not an X handle (no @)", h)
			}
		}
		if len(f.Query) > maxQuery {
			return fmt.Errorf("query: at most %d bytes", maxQuery)
		}
		if f.LookbackHours == 0 {
			f.LookbackHours = defaultLookbackHours
		}
		if f.LookbackHours < 1 || f.LookbackHours > maxLookbackHours {
			return fmt.Errorf("lookback_hours must be 1–%d", maxLookbackHours)
		}
		if f.IntervalHours == 0 {
			f.IntervalHours = defaultIntervalHours
		}
		if f.IntervalHours < 1 || f.IntervalHours > maxLookbackHours {
			return fmt.Errorf("interval_hours must be 1–%d", maxLookbackHours)
		}
		if f.Model == "" {
			f.Model = defaultXModel
		}
		if _, ok := xaiPrices[f.Model]; !ok {
			return fmt.Errorf("model %q has no price in xaiPrices: its spend cannot be estimated", f.Model)
		}
		if f.MaxTurns == 0 {
			f.MaxTurns = defaultMaxTurns
		}
		if f.MaxTurns < 1 || f.MaxTurns > maxMaxTurns {
			return fmt.Errorf("max_turns must be 1–%d", maxMaxTurns)
		}
	case KindDiscord:
		if f.URL != "" || xs || f.TitleFromSummary {
			return errors.New("url/xsearch/title_from_summary fields on a discord source")
		}
		if !snowflakeRe.MatchString(f.ChannelID) || !snowflakeRe.MatchString(f.GuildID) {
			return errors.New("discord needs channel_id and guild_id (numeric snowflakes)")
		}
	default:
		return fmt.Errorf("unknown kind %q (want rss, xsearch or discord)", f.Kind)
	}
	return nil
}
