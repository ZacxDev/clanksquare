// Command editor groups the curator's raw channel items into stories, has a
// decision model choose which to post, and posts them to top-<topic>
// channels. It is one-shot: run it on a schedule. See editor/.
//
//	editor measure     -config c.json                       # read-only, no model
//	editor run         -config c.json -key k -state dir     # dry run (default)
//	editor run         -config c.json -key k -state dir -post
//	editor label-draft -config c.json -out labels.jsonl -items 200 -pairs 200
//	editor eval        -config c.json -labels labels.jsonl [-floor-f1 0.5]
//	editor agree       -config c.json [-state dir]          # read-only
//	editor genkey      <path>
//
// The OpenRouter key comes from $OPENROUTER_API_KEY or -openrouter-key-file;
// it is never a command-line value. Exit status: 0 ok, 1 failed, 2 bad usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ZacxDev/clanksquare/editor"
)

func main() { os.Exit(run(os.Args[1:])) }

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: editor measure|run|label-draft|eval|agree|genkey [flags] (editor <cmd> -h)")
	return 2
}

func run(args []string) int {
	if len(args) == 0 {
		return usage()
	}
	cmd, args := args[0], args[1:]
	if cmd == "genkey" {
		if len(args) != 1 {
			return usage()
		}
		id, err := editor.GenerateKeyFile(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			return 1
		}
		fmt.Println(id)
		return 0
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := fs.String("config", "", "config JSON file (required)")
	keyFile := fs.String("openrouter-key-file", "", "file holding the OpenRouter key (used when $OPENROUTER_API_KEY is unset)")
	ledger := fs.String("spend-ledger", "", "JSON file accumulating model spend across runs; the cap applies to its total")
	maxSpend := fs.Float64("max-spend", 0, "override max_spend_usd")
	var o editor.CmdOpts
	switch cmd {
	case "measure":
	case "run":
		fs.StringVar(&o.KeyPath, "key", "", "editor key file (required)")
		fs.StringVar(&o.StateDir, "state", "", "state directory (cluster state, pending posts, lock)")
		fs.BoolVar(&o.Post, "post", false, "actually submit to the hub; without it the run is a dry run that submits nothing and writes no state")
	case "label-draft":
		fs.StringVar(&o.Out, "out", "", "JSONL file to write (required)")
		fs.IntVar(&o.NItems, "items", 200, "item labels to draft")
		fs.IntVar(&o.NPairs, "pairs", 200, "pair labels to draft")
		fs.StringVar(&o.LabelSource, "source", "llm-papers", "raw channel to sample items from")
	case "eval":
		fs.StringVar(&o.Labels, "labels", "", "labels JSONL (required)")
		fs.Float64Var(&o.FloorF1, "floor-f1", 0, "exit 1 if any label's F1 (with support) is below this")
		fs.Float64Var(&o.FloorPairF1, "floor-pair-f1", 0, "exit 1 if pairwise cluster F1 is below this")
		fs.BoolVar(&o.HumanOnly, "human-only", false, "score only rows with a human_label (else human_label, falling back to model_label)")
	case "agree":
		fs.StringVar(&o.StateDir, "state", "", "state directory written by run -post: its decisions are used where it has them")
		fs.IntVar(&o.MinPoints, "min-points", 100, "HN points that make an item 'popular'")
		fs.IntVar(&o.MinAgeHours, "min-age-hours", 48, "only items at least this old")
		fs.IntVar(&o.MaxAgeHours, "max-age-hours", 168, "only items at most this old")
		fs.BoolVar(&o.NoDecide, "no-decide", false, "do not ask the decision model about popular items the state has no decision for")
	default:
		return usage()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "editor: -config is required")
		return 2
	}
	cfg, err := editor.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "editor:", err)
		return 2
	}
	if *maxSpend > 0 {
		cfg.MaxSpendUSD = *maxSpend
	}
	o.Config = cfg
	o.KeyFile = *keyFile
	o.Ledger = *ledger
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return editor.Command(ctx, cmd, o, os.Stdout)
}
