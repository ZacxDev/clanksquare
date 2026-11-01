package curator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is a cache of what the curator has posted. The channel log is the
// truth: a channel with no entry here is rebuilt from its log (rebuildSeen).
//
// File format (JSON, version 1):
//
//	{"version":1,"channels":{"c_…":{"slug":"…","seen":{"<dedupe key>":<first-seen unix ms>}}}}
type State struct {
	Version  int                      `json:"version"`
	Channels map[string]*ChannelState `json:"channels"`
}

// ChannelState is one channel's seen set, keyed by channel id so a slug that
// comes to name a different channel never inherits another channel's set.
type ChannelState struct {
	Slug string           `json:"slug"`
	Seen map[string]int64 `json:"seen"`
}

func newState() *State { return &State{Version: 1, Channels: map[string]*ChannelState{}} }

// LoadState reads a state file. A missing file returns (nil, nil): there is
// nothing cached and every channel is rebuilt from its log. An unreadable or
// malformed file is an error the caller treats the same way, loudly.
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
	if s.Channels == nil {
		s.Channels = map[string]*ChannelState{}
	}
	for id, cs := range s.Channels {
		if cs == nil || cs.Seen == nil {
			delete(s.Channels, id) // rebuilt from the log
		}
	}
	return &s, nil
}

// prune forgets seen keys first seen before cutoff (unix ms).
func (s *State) prune(cutoff int64) {
	for _, cs := range s.Channels {
		for k, at := range cs.Seen {
			if at < cutoff {
				delete(cs.Seen, k)
			}
		}
	}
}

// Save writes the state atomically: a temp file in the same directory,
// synced, then renamed over the old one. A crash leaves the old or the new
// file, never a partial one.
func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".curator-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
