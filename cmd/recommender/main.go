// Command recommender posts channel recommendations (new and active
// channels) to #recommendations, a channel it owns. It is one-shot: run it
// on a schedule (hourly). See recommender/ for what one run does, and
// README.md ("Channel recommender").
//
//	recommender -genkey recommender.key        # prints only the k_ key id
//	recommender -hub-url URL -key recommender.key -state state.json [-ensure-channel] [-dry-run]
//
// The key is read from a file (-key, or RECOMMENDER_KEY_FILE), never from
// the command line. Exit status: 0 clean, 1 the run failed, 2 bad usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
	"github.com/ZacxDev/clanksquare/recommender"
)

func main() { os.Exit(run()) }

func run() int {
	hubURL := flag.String("hub-url", os.Getenv("RECOMMENDER_HUB_URL"), "hub base URL, e.g. https://clankmail.zacx.dev (env RECOMMENDER_HUB_URL)")
	keyPath := flag.String("key", os.Getenv("RECOMMENDER_KEY_FILE"), "key file: base64url of a 32-byte Ed25519 seed (env RECOMMENDER_KEY_FILE)")
	statePath := flag.String("state", "", "state file (a cache; without it the cooldown is rebuilt from the channel log)")
	slug := flag.String("channel", recommender.DefaultSlug, "slug of the recommendations channel")
	maxPerRun := flag.Int("max", recommender.DefaultMaxPerRun, fmt.Sprintf("at most this many recommendations per run (1-%d)", recommender.MaxMaxPerRun))
	topActive := flag.Int("top-active", recommender.DefaultTopActive, fmt.Sprintf("consider this many of the most active channels (1-%d)", recommender.MaxTopActive))
	cooldown := flag.Duration("cooldown", recommender.DefaultCooldown, "do not recommend a channel again within this long")
	newWindow := flag.Duration("new-window", recommender.DefaultNewWindow, "a channel created within this long is new")
	ensure := flag.Bool("ensure-channel", false, "create the channel if absent (read public, post owner, join open); an existing one is never changed")
	dryRun := flag.Bool("dry-run", false, "select and print what would be posted; submit nothing, write no state")
	genkey := flag.String("genkey", "", "write a fresh key file (mode 0600, never overwrites) and print its k_ id")
	flag.Parse()

	if *genkey != "" {
		id, err := hubkit.GenerateKeyFile(*genkey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			return 1
		}
		fmt.Println(id)
		return 0
	}
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "recommender: unexpected arguments (the key is read from a file: -key or RECOMMENDER_KEY_FILE)")
		return 2
	}
	if *keyPath == "" {
		fmt.Fprintln(os.Stderr, "recommender: -key (or RECOMMENDER_KEY_FILE) is required (or -genkey <path>)")
		return 2
	}
	key, err := hubkit.LoadKeyFile(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recommender:", err)
		return 2
	}
	opts := recommender.Options{HubURL: *hubURL, Key: key, StatePath: *statePath, Slug: *slug,
		MaxPerRun: *maxPerRun, TopActive: *topActive, Cooldown: *cooldown, NewWindow: *newWindow,
		EnsureChannel: *ensure, DryRun: *dryRun}
	if err := opts.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "recommender:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return recommender.Run(ctx, opts).ExitCode()
}
