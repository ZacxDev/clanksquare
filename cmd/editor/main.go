// Command editor groups the curator's raw channel items into stories (by
// canonical URL), has a decision model rank them, and posts each day's best
// to top-<topic> channels. It is one-shot: run it on a schedule. See editor/.
//
//	editor measure     -config c.json                       # read-only, no model
//	editor run         -config c.json -key k -state dir     # dry run (default)
//	editor run         -config c.json -key k -state dir -preview  # dry run that keeps decisions + picks
//	editor run         -config c.json -key k -state dir -post
//	editor truth       -config c.json -state dir              # read-only: HF/HN popularity labels
//	editor eval        -config c.json -truth dir/truth.json -preview-dir dir/preview   # no model calls
//	editor eval        -config c.json -truth dir/truth.json [-since-hours 336]         # decides the hub window
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
	fmt.Fprintln(os.Stderr, "usage: editor measure|run|truth|eval|genkey [flags] (editor <cmd> -h)")
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
		fs.BoolVar(&o.Preview, "preview", false, "dry run that keeps its decisions and simulated picks in <state>/preview/ and appends them, with scores, to <state>/preview/preview-<UTC date>.jsonl; submits nothing")
	case "truth":
		fs.StringVar(&o.StateDir, "state", "", "directory holding the truth cache, truth.json (required)")
		fs.BoolVar(&o.Refresh, "refresh", false, "look up items the cache already labels, too")
	case "eval":
		fs.StringVar(&o.Truth, "truth", "", "popularity truth file written by editor truth (required for any score)")
		fs.StringVar(&o.PreviewDir, "preview-dir", "", "score the stories and P(post) in run -preview's preview-*.jsonl files here (no model calls); without it, decide the hub window")
		fs.IntVar(&o.SinceHours, "since-hours", 336, "hub window: read source items at most this many hours old")
		fs.Float64Var(&o.FloorAUC, "floor-auc", 0, "exit 1 if the AUC of P(post) against the popularity truth is below this")
		fs.Float64Var(&o.FloorPrecisionAtK, "floor-precision-at-k", 0, "exit 1 if the share of picked stories (top_k_per_day per UTC day) whose truth is post is below this")
		fs.Float64Var(&o.FloorRecallPopular, "floor-recall-popular", 0, "exit 1 if the share of popular (truth post) stories picked is below this")
		fs.StringVar(&o.Predictions, "predictions", "", "write each story's day, P(post), pick and truth to this JSONL file (never overwritten)")
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
