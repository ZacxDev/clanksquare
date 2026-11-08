package curator

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
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
	MaxAgeHours int       `json:"max_age_hours,omitempty"`
	Channels    []Channel `json:"channels"`
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
	KindHFDaily = "hfdaily" // Hugging Face Daily Papers, as arXiv links (hfdaily.go)
)

// FeedConfig is one source and its keyword filters.
type FeedConfig struct {
	// Kind is rss (default) or hfdaily.
	Kind string `json:"kind,omitempty"`
	// URL is the feed URL (rss only).
	URL string `json:"url,omitempty"`
	// Name, when set, replaces the feed's own title in the "via" line
	// (not allowed on hfdaily, whose via line is always HFDailySource).
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

	// hfdaily: how many UTC dates to read, today included (default 2, ≤ 7).
	LookbackDays int `json:"lookback_days,omitempty"`
}

// label names a source in logs. For rss it is the URL, as before.
func (f FeedConfig) label() string {
	if f.Kind == KindHFDaily {
		return "hfdaily"
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

// validate fills a source's defaults and checks it. Fields that belong to
// another kind are errors, so a misplaced field cannot silently do nothing.
func (f *FeedConfig) validate() error {
	if f.Kind == "" {
		f.Kind = KindRSS
	}
	if f.MaxPerRun < 0 || f.MaxPerRun > maxMaxPerRun {
		return fmt.Errorf("max_per_run must be 0–%d", maxMaxPerRun)
	}
	hf := f.LookbackDays != 0
	switch f.Kind {
	case KindRSS:
		if hf {
			return errors.New("hfdaily fields on an rss feed")
		}
		if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("feed url %q: want an http(s) URL", f.URL)
		}
	case KindHFDaily:
		// No name: the via label must stay HFDailySource (see hfdaily.go).
		if f.Name != "" {
			return errors.New("name on an hfdaily source: its via label is always " + HFDailySource)
		}
		if f.URL != "" || f.TitleFromSummary {
			return errors.New("url/title_from_summary fields on an hfdaily source")
		}
		if f.LookbackDays == 0 {
			f.LookbackDays = defaultLookbackDays
		}
		if f.LookbackDays < 1 || f.LookbackDays > maxLookbackDays {
			return fmt.Errorf("lookback_days must be 1–%d", maxLookbackDays)
		}
	default:
		return fmt.Errorf("unknown kind %q (want rss or hfdaily)", f.Kind)
	}
	return nil
}
