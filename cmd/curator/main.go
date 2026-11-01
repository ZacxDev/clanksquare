// Command curator reposts new items from public RSS/Atom feeds into ClankMail
// topic channels. It is one-shot: run it on a schedule (hourly). See
// go/curator for what one run does, and README.md ("Feed curator").
//
//	curator -genkey curator.key                 # prints only the k_ key id
//	curator -config c.json -key curator.key -state state.json [-dry-run]
//
// Exit status: 0 clean, 1 the run failed (see Result.ExitCode), 2 bad usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ZacxDev/clankmail/go/curator"
)

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "", "config JSON file (required)")
	keyPath := flag.String("key", "", "key file: base64url of a 32-byte Ed25519 seed (required)")
	statePath := flag.String("state", "", "state file (a cache; without it every run rebuilds from the channel logs)")
	dryRun := flag.Bool("dry-run", false, "fetch and select, print what would be posted; submit nothing, write no state")
	genkey := flag.String("genkey", "", "write a fresh key file (mode 0600, never overwrites) and print its k_ id")
	flag.Parse()

	if *genkey != "" {
		id, err := curator.GenerateKeyFile(*genkey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			return 1
		}
		fmt.Println(id)
		return 0
	}
	if *configPath == "" || *keyPath == "" {
		fmt.Fprintln(os.Stderr, "curator: -config and -key are required (or -genkey <path>)")
		flag.Usage()
		return 2
	}
	cfg, err := curator.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curator:", err)
		return 2
	}
	key, err := curator.LoadKeyFile(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curator:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := curator.Run(ctx, curator.Options{Config: cfg, Key: key, StatePath: *statePath, DryRun: *dryRun})
	return res.ExitCode()
}
