package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "Print what would be removed without deleting anything")
	apply := flag.Bool("apply", false, "Actually delete orphaned batch directories")
	sandmanDir := flag.String("sandman-dir", ".sandman", "Path to the .sandman directory containing batches/ and events.jsonl")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "cleanup-orphaned-batches removes orphaned test batch directories under <sandman-dir>/batches/.\n\n")
		fmt.Fprintf(os.Stderr, "A batch directory is orphaned when it has a batch.json manifest, no run.started event references it, and no live daemon socket is bound to it.\n\n")
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if !*dryRun && !*apply {
		fmt.Fprintln(os.Stderr, "error: must pass --dry-run or --apply")
		flag.Usage()
		os.Exit(2)
	}
	if *dryRun && *apply {
		fmt.Fprintln(os.Stderr, "error: --dry-run and --apply are mutually exclusive")
		flag.Usage()
		os.Exit(2)
	}

	log := &events.JSONLLogger{Path: filepath.Join(*sandmanDir, "events.jsonl")}

	var removed []string
	var err error
	mode := "removed"
	if *dryRun {
		removed, err = daemon.PlanOrphanedTestBatches(*sandmanDir, log, daemon.IsRunActive)
		mode = "would remove"
	} else {
		removed, err = daemon.CleanupOrphanedTestBatches(*sandmanDir, log, daemon.IsRunActive)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(removed) == 0 {
		fmt.Println("No orphaned batch directories found.")
		return
	}
	fmt.Printf("%s %d orphaned batch director(ies):\n", mode, len(removed))
	for _, p := range removed {
		fmt.Printf("  - %s\n", p)
	}
}