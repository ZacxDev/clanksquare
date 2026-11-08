package editor

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"os"
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
	Preview  bool // run: unattended dry run that keeps its decisions and picks (RunOptions.Preview)

	Truth              string  // eval: popularity truth file (editor truth's cache)
	Refresh            bool    // truth: look up rows already labelled
	PreviewDir         string  // eval: score run -preview's files instead of deciding the hub window
	SinceHours         int     // eval: the hub window's age limit
	Predictions        string  // eval: per-story JSONL output
	FloorAUC           float64 // eval floors
	FloorPrecisionAtK  float64
	FloorRecallPopular float64

	// Now is eval's clock (the hub window's age limit); nil means time.Now.
	Now func() time.Time

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
		res := Run(ctx, RunOptions{Config: o.Config, Key: key, StateDir: o.StateDir, Post: o.Post, Preview: o.Preview, Hub: hub, Model: model, Out: out})
		fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		return res.ExitCode()
	case "eval":
		eo := EvalOptions{Config: o.Config, PreviewDir: o.PreviewDir, Hub: hub, SinceHours: o.SinceHours, Now: o.Now, Log: out}
		var model *Model
		if o.PreviewDir == "" {
			m, err := newModel()
			if err != nil {
				fmt.Fprintln(out, "ERROR", err)
				return 2
			}
			model, eo.Model = m, m
		}
		if o.Truth != "" {
			tr, err := ReadTruth(o.Truth)
			if err != nil {
				fmt.Fprintln(out, "ERROR", err)
				return 2
			}
			eo.Truth = tr
		}
		if o.Predictions != "" {
			pf, err := os.OpenFile(o.Predictions, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				fmt.Fprintln(out, "ERROR", err)
				return 2
			}
			defer pf.Close()
			eo.Predictions = pf
		}
		rep, err := Eval(ctx, eo)
		if model != nil {
			defer fmt.Fprintf(out, "spend: $%.6f over %d call(s) (cap $%.4f)\n", model.Meter.Spent(), model.Meter.Calls(), model.Meter.Cap)
		}
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		rep.Write(out)
		if fails := rep.BelowFloors(Floors{AUC: o.FloorAUC, PrecisionAtK: o.FloorPrecisionAtK, RecallPopular: o.FloorRecallPopular}); len(fails) > 0 {
			for _, f := range fails {
				fmt.Fprintln(out, "FAIL", f)
			}
			return 1
		}
		return 0
	case "truth":
		rep, err := Truth(ctx, TruthOptions{Config: o.Config, Hub: hub, HTTP: o.HTTP, StateDir: o.StateDir, Refresh: o.Refresh,
			Delay: 250 * time.Millisecond, Log: out})
		if err != nil {
			fmt.Fprintln(out, "ERROR", err)
			return 1
		}
		rep.Write(out)
		fmt.Fprintf(out, "wrote %s\n", TruthPath(o.StateDir))
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
