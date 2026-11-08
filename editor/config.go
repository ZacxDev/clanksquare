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
	DecisionModel string `json:"decision_model"`
	// Prices are USD per million tokens [input, output] for each model the
	// editor may call. A call to a model with no price is refused: without
	// one the spend cap cannot be enforced.
	Prices map[string][2]float64 `json:"prices"`
	// MaxSpendUSD caps model spend per run (and per ledger, see SpendLedger).
	MaxSpendUSD float64 `json:"max_spend_usd"`

	Cluster ClusterConfig `json:"cluster"`
	Decide  DecideConfig  `json:"decide"`
	Truth   TruthConfig   `json:"truth"`
	Topics  []Topic       `json:"topics"`
}

// TruthConfig maps outside popularity signals to labels for `editor truth`
// (see truth.go). A value at or above *Post is post, at or above *Digest is
// digest, below is drop. Items younger than MinAgeHours, and items with no
// signal at all, are unknown.
type TruthConfig struct {
	MinAgeHours int `json:"min_age_hours"`
	// RecheckHours: an item whose lookup answered "no signal" (not on HF,
	// not on Daily Papers, not on HN, not settled yet) is not looked up again
	// until this many hours after that answer (default 20, so a daily run
	// re-checks it about once a day). A failed lookup is retried every run.
	RecheckHours int `json:"recheck_hours,omitempty"`
	// HF Daily Papers upvotes, for arXiv papers.
	HFPost   int `json:"hf_post"`
	HFDigest int `json:"hf_digest"`
	// HN points (Algolia), for everything else.
	HNPost   int `json:"hn_post"`
	HNDigest int `json:"hn_digest"`
	// ReleaseLabel is the fixed label of a GitHub release page: there is no
	// cheap popularity signal for one (release reactions need an API token
	// to read at any volume).
	ReleaseLabel string `json:"release_label"`
}

// ClusterConfig tunes story clustering (by canonical URL, see cluster.go).
type ClusterConfig struct {
	// WindowHours: an item joins a story with its canonical URL only within
	// this many hours of the story's first item (default 72).
	WindowHours int `json:"window_hours"`
}

// DecideConfig holds the decision settings.
//
// What a run POSTS is chosen by rank, not by an absolute threshold: each top
// channel posts at most TopKPerDay stories per UTC day, the eligible stories
// with the highest P(post) (ties by story id), and only those with P(post) >=
// MinPPost (0 = no floor). A story is eligible while its first item is less
// than WindowHours old and it has not been posted; one not picked today is
// ranked again tomorrow against that day's stories, until it leaves the window
// (see pick.go). eval scores this pick (eval.go).
type DecideConfig struct {
	// TopKPerDay is the per-channel daily post budget (default 4).
	TopKPerDay int `json:"top_k_per_day,omitempty"`
	// MinPPost is an optional absolute floor on P(post) for a pick (default
	// 0: off), so a day with nothing good posts fewer than TopKPerDay.
	MinPPost float64 `json:"min_p_post,omitempty"`
	// WindowHours: a story is eligible for a pick while its first item is
	// younger than this (default 72).
	WindowHours int `json:"window_hours,omitempty"`
	// Question is the wording asked: "v1" (topicality and notability) or
	// "v2" (the rubric, framed as notability for agent and LLM builders).
	Question string `json:"question,omitempty"`
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
	From []string `json:"from"`
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
	if c.DecisionModel == "" {
		c.DecisionModel = DefaultDecisionModel
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
	if c.Cluster.WindowHours == 0 {
		c.Cluster.WindowHours = 72
	}
	if c.Decide.Question == "" {
		c.Decide.Question = "v2"
	}
	if c.Decide.TopKPerDay == 0 {
		c.Decide.TopKPerDay = 4
	}
	if c.Decide.WindowHours == 0 {
		c.Decide.WindowHours = 72
	}
	if c.Truth.MinAgeHours == 0 {
		c.Truth.MinAgeHours = 48
	}
	if c.Truth.RecheckHours == 0 {
		c.Truth.RecheckHours = 20
	}
	if c.Truth.HFPost == 0 {
		c.Truth.HFPost = DefaultHFPost
	}
	if c.Truth.HFDigest == 0 {
		c.Truth.HFDigest = DefaultHFDigest
	}
	if c.Truth.HNPost == 0 {
		c.Truth.HNPost = 100
	}
	if c.Truth.HNDigest == 0 {
		c.Truth.HNDigest = 10
	}
	if c.Truth.ReleaseLabel == "" {
		c.Truth.ReleaseLabel = "digest"
	}
}

// DefaultDecisionModel is the decider a config gets when it names none:
// Perplexity Decider v1.2 with question v2. In the 2026-11-08 bake-off (314
// #llm-papers items, 29 with popularity truth, 7 of them post) it had the best
// AUC of P(post), 0.799, and the only bootstrap 95% interval excluding 0.5
// ([0.62, 0.94]); its P(post) never reached the old 0.6 gate, which is why
// posting is by rank (DecideConfig).
const DefaultDecisionModel = "perplexity/pplx-decider-v1.2"

// DefaultHFPost and DefaultHFDigest are the HF Daily Papers upvote
// thresholds. Measured 2026-11-08 over every paper HF listed on Daily Papers
// from 2026-10-30 to 2026-11-08 (n=245: p50 12, p75 25, p85 38, p90 48,
// max 144): 40 makes post the top 14.7% (36 of 245) of the papers HF readers
// were shown, inside the 10-15% target; 10 sends the bottom ~40%, which got
// almost no votes in front of the same readers, to drop. They were picked from
// the Daily Papers population, not from the 29 #llm-papers items on it, so the
// cut does not depend on the sample being scored.
const (
	DefaultHFPost   = 40
	DefaultHFDigest = 10
)

// DefaultPrices are the OpenRouter list prices (USD per million tokens,
// [input, output]) read from /api/v1/models on 2026-11-08, for the Decisions
// API models: the default decider and the bake-off's other candidate.
var DefaultPrices = map[string][2]float64{
	"perplexity/pplx-decider-v1.2":  {0.02, 0},
	"scaledown/classify-1-20260928": {0.04, 0},
	"scaledown/classify-1":          {0.04, 0},
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
	if c.Cluster.WindowHours < 1 || c.Cluster.WindowHours > 24*14 {
		return fmt.Errorf("cluster: window_hours must be 1-336")
	}
	if c.Decide.TopKPerDay < 1 || c.Decide.TopKPerDay > 50 {
		return fmt.Errorf("decide: top_k_per_day must be 1-50")
	}
	if c.Decide.MinPPost < 0 || c.Decide.MinPPost > 1 {
		return fmt.Errorf("decide: min_p_post must be in [0, 1]")
	}
	if c.Decide.WindowHours < 1 || c.Decide.WindowHours > 24*14 {
		return fmt.Errorf("decide: window_hours must be 1-336")
	}
	if c.Truth.RecheckHours < 1 {
		return fmt.Errorf("truth: recheck_hours must be >= 1")
	}
	if c.Decide.Question != "v1" && c.Decide.Question != "v2" {
		return fmt.Errorf("decide: question must be v1 or v2")
	}
	if t := c.Truth; t.HFDigest < 1 || t.HFPost <= t.HFDigest || t.HNDigest < 1 || t.HNPost <= t.HNDigest {
		return fmt.Errorf("truth: want 1 <= *_digest < *_post")
	}
	if !contains(Labels, c.Truth.ReleaseLabel) {
		return fmt.Errorf("truth: release_label must be post, digest or drop")
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
