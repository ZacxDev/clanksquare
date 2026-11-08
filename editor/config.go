package editor

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Config is the editor's JSON config. See cmd/editor/example-config.json.
type Config struct {
	Hub    string `json:"hub"`     // the hub's name, for the handle (unused when Handle is "")
	HubURL string `json:"hub_url"` // base URL of the hub API
	// Sources are the raw channels read, by slug. Posts in them signed by
	// anyone but the channel's owner are ignored.
	Sources []string `json:"sources"`
	// SourceOwner, when set, is the k_ key every source channel must be
	// owned by (the curator's). A source owned by another key fails the run.
	SourceOwner string `json:"source_owner,omitempty"`
	// MaxAgeHours: items older than this are not clustered or decided.
	MaxAgeHours int `json:"max_age_hours"`

	OpenRouterURL string `json:"openrouter_url"` // default https://openrouter.ai
	EmbedModel    string `json:"embed_model"`
	DecisionModel string `json:"decision_model"`
	LabelModel    string `json:"label_model"`
	// Prices are USD per million tokens [input, output] for each model the
	// editor may call. A call to a model with no price is refused: without
	// one the spend cap cannot be enforced.
	Prices map[string][2]float64 `json:"prices"`
	// MaxSpendUSD caps model spend per run (and per ledger, see SpendLedger).
	MaxSpendUSD float64 `json:"max_spend_usd"`

	Cluster ClusterConfig `json:"cluster"`
	Decide  DecideConfig  `json:"decide"`
	Topics  []Topic       `json:"topics"`
}

// ClusterConfig tunes story clustering.
type ClusterConfig struct {
	// Threshold: an item joins the most similar open cluster whose centroid
	// has cosine similarity >= Threshold.
	Threshold float64 `json:"threshold"`
	// Borderline: with AskBorderline, a best similarity in
	// [Borderline, Threshold) is settled by a "same story?" decision.
	Borderline    float64 `json:"borderline"`
	AskBorderline bool    `json:"ask_borderline"`
	// SameStoryMin is the noul probability at or above which a borderline
	// pair is the same story.
	SameStoryMin float64 `json:"same_story_min"`
	WindowHours  int     `json:"window_hours"`
}

// DecideConfig holds the decision thresholds. Classify-1 always returns a
// winner, so the gate is on probabilities, not on the winner: a story is
// posted only if P(post) >= Post, sent to the digest only if P(digest) >=
// Digest, and dropped otherwise.
type DecideConfig struct {
	Post   float64 `json:"post"`
	Digest float64 `json:"digest"`
}

// Topic is one top-<topic> channel the editor owns.
type Topic struct {
	Slug  string   `json:"slug"`
	Title string   `json:"title"`
	About string   `json:"about"`
	Tags  []string `json:"tags,omitempty"`
	// Describe is the topic as the decision model reads it.
	Describe string `json:"describe"`
	// From are the source slugs whose stories this topic considers: a story
	// qualifies if any of its items came from one of them.
	From      []string `json:"from"`
	MaxPerRun int      `json:"max_per_run"`
}

// slugRE is SPEC.md §7.1's slug rule.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// The about text must say a model selects the items and who runs the bot.
// These are checked, not just documented, so a config edit cannot quietly
// drop the disclosure.
var aboutMustSay = []string{"model", "operator"}

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.MaxAgeHours == 0 {
		c.MaxAgeHours = 48
	}
	if c.OpenRouterURL == "" {
		c.OpenRouterURL = "https://openrouter.ai"
	}
	if c.EmbedModel == "" {
		c.EmbedModel = "openai/text-embedding-3-small"
	}
	if c.DecisionModel == "" {
		c.DecisionModel = "scaledown/classify-1-20260928"
	}
	if c.LabelModel == "" {
		c.LabelModel = "anthropic/claude-sonnet-5.5-20260928"
	}
	if c.Prices == nil {
		c.Prices = map[string][2]float64{}
	}
	for m, p := range DefaultPrices {
		if _, ok := c.Prices[m]; !ok {
			c.Prices[m] = p
		}
	}
	if c.MaxSpendUSD == 0 {
		c.MaxSpendUSD = 0.10
	}
	if c.Cluster.Threshold == 0 {
		c.Cluster.Threshold = 0.82
	}
	if c.Cluster.Borderline == 0 {
		c.Cluster.Borderline = 0.72
	}
	if c.Cluster.SameStoryMin == 0 {
		c.Cluster.SameStoryMin = 0.7
	}
	if c.Cluster.WindowHours == 0 {
		c.Cluster.WindowHours = 72
	}
	if c.Decide.Post == 0 {
		c.Decide.Post = 0.6
	}
	if c.Decide.Digest == 0 {
		c.Decide.Digest = 0.5
	}
	for i := range c.Topics {
		if c.Topics[i].MaxPerRun == 0 {
			c.Topics[i].MaxPerRun = 5
		}
	}
}

// DefaultPrices are the OpenRouter list prices (USD per million tokens,
// [input, output]) read from /api/v1/models on 2026-11-08.
var DefaultPrices = map[string][2]float64{
	"openai/text-embedding-3-small":        {0.02, 0},
	"scaledown/classify-1-20260928":        {0.04, 0},
	"scaledown/classify-1":                 {0.04, 0},
	"anthropic/claude-sonnet-5.5-20260928": {2, 10},
	"anthropic/claude-sonnet-5.5":          {2, 10},
	"anthropic/claude-haiku-5.5-20261007":  {0.1, 0.5},
	"anthropic/claude-haiku-5.5":           {0.1, 0.5},
}

// Validate checks a config after Defaults.
func (c *Config) Validate() error {
	if !strings.HasPrefix(c.HubURL, "http://") && !strings.HasPrefix(c.HubURL, "https://") {
		return fmt.Errorf("hub_url must be an http(s) URL")
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("sources: at least one raw channel is required")
	}
	for _, s := range c.Sources {
		if !slugRE.MatchString(s) {
			return fmt.Errorf("sources: bad slug %q", s)
		}
	}
	if !(c.Cluster.Borderline <= c.Cluster.Threshold && c.Cluster.Threshold <= 1) {
		return fmt.Errorf("cluster: want borderline <= threshold <= 1")
	}
	if c.Decide.Post <= 0 || c.Decide.Post > 1 || c.Decide.Digest <= 0 || c.Decide.Digest > 1 {
		return fmt.Errorf("decide: thresholds must be in (0, 1]")
	}
	src := map[string]bool{}
	for _, s := range c.Sources {
		src[s] = true
	}
	seen := map[string]bool{}
	for _, t := range c.Topics {
		if !slugRE.MatchString(t.Slug) || !strings.HasPrefix(t.Slug, "top-") {
			return fmt.Errorf("topic %q: slug must be top-<topic>", t.Slug)
		}
		if seen[t.Slug] || src[t.Slug] {
			return fmt.Errorf("topic %q: duplicate, or also a source", t.Slug)
		}
		seen[t.Slug] = true
		if t.Title == "" || t.Describe == "" || len(t.From) == 0 {
			return fmt.Errorf("topic %q: title, describe and from are required", t.Slug)
		}
		low := strings.ToLower(t.About)
		for _, w := range aboutMustSay {
			if !strings.Contains(low, w) {
				return fmt.Errorf("topic %q: about must say that a model selects the items and that the operator runs the bot (missing %q)", t.Slug, w)
			}
		}
		for _, f := range t.From {
			if !src[f] {
				return fmt.Errorf("topic %q: from %q is not a source", t.Slug, f)
			}
		}
	}
	return nil
}

// LoadConfig reads, defaults and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}
