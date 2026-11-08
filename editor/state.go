package editor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// State is the editor's state file, <state dir>/state.json:
//
//	{"version":1,
//	 "items":   {"m_…": "s_…"},                 every raw item ever assigned, and its story
//	 "stories": {"s_…": Story},
//	 "channels":{"c_…": {"slug":"top-…","seen":{"s_…":<ms>},"pending":{"s_…":"<token>"}}}}
//
// Story state is the editor's own: unlike the curator's seen set it cannot
// be rebuilt from a log, because the stories' membership and decisions are
// not in any post. A lost state file means items are regrouped (and stories
// decided again); the top channels' seen sets are rebuilt from their logs, so
// a story is still never posted twice.
//
// Pending is the curator's write-ahead pattern: a token is saved before it
// is submitted and replaced by a seen entry once the hub confirms it, so a
// crash between the two resends the same bytes instead of posting again.
type State struct {
	Version  int                      `json:"version"`
	Items    map[string]string        `json:"items"`
	Stories  map[string]*Story        `json:"stories"`
	Channels map[string]*ChannelState `json:"channels"`
}

// ChannelState is one top channel's posted stories.
type ChannelState struct {
	Slug    string            `json:"slug"`
	Seen    map[string]int64  `json:"seen"`
	Pending map[string]string `json:"pending,omitempty"`
}

// Story is one cluster of raw items about the same thing.
type Story struct {
	ID      string      `json:"id"`
	Created int64       `json:"created"` // ts of its first item, unix ms
	Last    int64       `json:"last"`
	Canon   []string    `json:"canon"`
	Items   []StoryItem `json:"items"`
	// Decisions are keyed by topic slug.
	Decisions map[string]*StoryDecision `json:"decisions,omitempty"`
}

// StoryItem is the part of a raw item a story keeps.
type StoryItem struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	TS      int64  `json:"ts"`
	Title   string `json:"title"`
	Link    string `json:"link"`
	Canon   string `json:"canon,omitempty"`
	Via     string `json:"via,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// StoryDecision is a decision model's verdict on a story for one topic.
type StoryDecision struct {
	Raw   string             `json:"raw"` // the model's own winner
	Probs map[string]float64 `json:"probs"`
	Model string             `json:"model"`
	At    int64              `json:"at"`
}

func newState() *State {
	return &State{Version: 1, Items: map[string]string{}, Stories: map[string]*Story{}, Channels: map[string]*ChannelState{}}
}

// StatePath is the state file in dir.
func StatePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState reads dir's state file; a missing file is a new state.
func LoadState(dir string) (*State, error) {
	raw, err := os.ReadFile(StatePath(dir))
	if os.IsNotExist(err) {
		return newState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := newState()
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("state %s: %w", StatePath(dir), err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("state %s: unsupported version %d", StatePath(dir), s.Version)
	}
	if s.Items == nil {
		s.Items = map[string]string{}
	}
	if s.Stories == nil {
		s.Stories = map[string]*Story{}
	}
	if s.Channels == nil {
		s.Channels = map[string]*ChannelState{}
	}
	for id, cs := range s.Channels {
		if cs == nil {
			delete(s.Channels, id)
			continue
		}
		if cs.Seen == nil {
			cs.Seen = map[string]int64{}
		}
	}
	return s, nil
}

// Save writes the state atomically.
func (s *State) Save(dir string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return hubkit.WriteFileAtomic(StatePath(dir), append(raw, '\n'))
}
