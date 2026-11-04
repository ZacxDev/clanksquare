package recommender

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// State is a cache of what the recommender has posted, for the cooldown. The
// #recommendations log is the truth: with no state file, the cooldown is
// rebuilt from the recommender's own verified posts there.
//
// File format (JSON, version 1):
//
//	{"version":1,"channel":"c_…","recommended":{"c_…":{"slug":"…","at":<unix ms>}}}
//
// channel is the recommendations channel the entries were posted to; a state
// file for a different channel is ignored (rebuilt from the log).
type State struct {
	Version     int                  `json:"version"`
	Channel     string               `json:"channel"`
	Recommended map[string]RecRecord `json:"recommended"`
}

// RecRecord is the last time a channel was recommended.
type RecRecord struct {
	Slug string `json:"slug"`
	At   int64  `json:"at"`
}

func newState(channel string) *State {
	return &State{Version: 1, Channel: channel, Recommended: map[string]RecRecord{}}
}

// LoadState reads a state file. A missing file is (nil, nil).
func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("state %s: unsupported version %d", path, s.Version)
	}
	if s.Recommended == nil {
		s.Recommended = map[string]RecRecord{}
	}
	return &s, nil
}

// Save writes the state atomically (mode 0600).
func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return hubkit.WriteFileAtomic(path, append(raw, '\n'))
}
