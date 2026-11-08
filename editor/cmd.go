package editor

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// CmdOpts are the parsed flags of one subcommand.
type CmdOpts struct {
	Config   *Config
	KeyFile  string // OpenRouter key file
	Ledger   string // spend ledger file
	KeyPath  string // editor signing key
	StateDir string
	Post     bool // run: submit for real

	Out         string
	NItems      int
	NPairs      int
	LabelSource string

	Labels      string
	FloorF1     float64
	FloorPairF1 float64
	HumanOnly   bool

	MinPoints   int
	MinAgeHours int
	MaxAgeHours int
	NoDecide    bool

	// HTTP is used for every request; nil means a default client.
	HTTP *http.Client
}

// GenerateKeyFile writes a fresh editor key (see hubkit.GenerateKeyFile).
func GenerateKeyFile(path string) (string, error) { return hubkit.GenerateKeyFile(path) }

// LoadKeyFile reads an editor key.
func LoadKeyFile(path string) (ed25519.PrivateKey, error) { return hubkit.LoadKeyFile(path) }

// Command runs one subcommand and returns its exit status.
func Command(ctx context.Context, cmd string, o CmdOpts, out io.Writer) int {
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	hub := NewHub(o.Config.HubURL, o.HTTP)
	newModel := func() (*Model, error) {
		key, err := LoadAPIKey(o.KeyFile)
		if err != nil {
			return nil, err
		}
		meter, err := NewMeter(o.Config.MaxSpendUSD, o.Ledger)
		if err != nil {
			return nil, err
		}
		return &Model{Base: o.Config.OpenRouterURL, Key: key, HTTP: o.HTTP, Meter: meter, Prices: o.Config.Prices}, nil
	}
	switch cmd {
	case "measure":
		items, err := ReadSources(ctx, hub, o.Config, out)
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		MeasureItems(items, o.Config.Sources).Write(out)
		return 0
	case "run":
		if o.KeyPath == "" {
			fmt.Fprintln(out, "ERROR run: -key is required")
			return 2
		}
		key, err := LoadKeyFile(o.KeyPath)
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 2
		}
		model, err := newModel()
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 2
		}
		res := Run(ctx, RunOptions{Config: o.Config, Key: key, StateDir: o.StateDir, Post: o.Post, Hub: hub, Model: model, Out: out})
		fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		return res.ExitCode()
	case "label-draft":
		if o.Out == "" {
			fmt.Fprintln(out, "ERROR label-draft: -out is required")
			return 2
		}
		model, err := newModel()
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 2
		}
		err = LabelDraft(ctx, LabelOptions{Config: o.Config, Hub: hub, Model: model, Out: o.Out, Source: o.LabelSource, NItems: o.NItems, NPairs: o.NPairs, Log: out})
		fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		return 0
	case "eval":
		if o.Labels == "" {
			fmt.Fprintln(out, "ERROR eval: -labels is required")
			return 2
		}
		model, err := newModel()
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 2
		}
		rep, err := Eval(ctx, EvalOptions{Config: o.Config, Model: model, Labels: o.Labels, HumanOnly: o.HumanOnly})
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		rep.Write(out)
		fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		if fails := rep.BelowFloors(o.FloorF1, o.FloorPairF1); len(fails) > 0 {
			for _, f := range fails {
				fmt.Fprintln(out, "FAIL", f)
			}
			return 1
		}
		return 0
	case "agree":
		ao := AgreeOptions{Config: o.Config, Hub: hub, StateDir: o.StateDir, HTTP: o.HTTP, MinPoints: o.MinPoints,
			MinAgeHours: o.MinAgeHours, MaxAgeHours: o.MaxAgeHours, Delay: 250 * time.Millisecond, Log: out}
		var model *Model
		if !o.NoDecide {
			m, err := newModel()
			if err != nil {
				fmt.Fprintln(out, "ERROR", err)
				return 2
			}
			model, ao.Model = m, m
		}
		rep, err := Agree(ctx, ao)
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		rep.Write(out)
		if model != nil {
			fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		}
		return 0
	}
	return 2
}

// ReadSources reads every configured raw channel, logging one line per
// channel, and returns the items oldest first.
func ReadSources(ctx context.Context, hub *hubkit.Client, cfg *Config, log io.Writer) ([]Item, error) {
	var all []Item
	for _, slug := range cfg.Sources {
		items, owner, ignored, err := ReadSource(ctx, hub, slug, cfg.SourceOwner)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(log, "INFO source=%s owner=%s items=%d ignored=%d\n", slug, owner, len(items), ignored)
		all = append(all, items...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].TS != all[j].TS {
			return all[i].TS < all[j].TS
		}
		return all[i].ID < all[j].ID
	})
	return all, nil
}
