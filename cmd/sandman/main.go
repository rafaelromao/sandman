package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"syscall"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/cmd"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// version is the build-time version string, injected via
// `go build -ldflags '-X main.version=<value>'` (see Makefile). The default
// is "dev" — the literal sentinel meaning "no ldflags injection happened."
var version = "dev"

// buildInfo is the package-level seam the unit tests use to stub
// runtime/debug.ReadBuildInfo. Production reads the linker-populated
// buildinfo (set by `go build` / `go install`).
var buildInfo = debug.ReadBuildInfo

// Version returns the sandman version string via the three-layer fallback
// chain: (1) the Makefile-injected ldflags value, when present; (2)
// runtime/debug.ReadBuildInfo().Main.Version, the linker-populated
// pseudo-version emitted by `go install ./cmd/sandman` without the Makefile;
// (3) the literal "dev" as the final fallback. The empty string and "dev"
// are both treated as "not yet injected" so `make build VERSION=dev` and a
// bare `go install` both flow through to the buildinfo fallback.
func Version() string {
	if v := version; v != "" && v != "dev" {
		return v
	}
	info, ok := buildInfo()
	if ok && info != nil && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

func isStdoutTTY() bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stdout.Fd()), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

// executeRoot runs the cobra command tree, routing returned errors to
// the right sink: ExitCodedError → exit code, UsageError → error + usage
// on stderr (using the failing subcommand's usage), anything else → plain
// error on stderr. Extracted from main so the behavior is testable without
// spawning a subprocess.
func executeRoot(rootCmd *cobra.Command, stderr io.Writer, exit func(int)) {
	failed, err := rootCmd.ExecuteC()
	if err != nil {
		var coded *cmd.ExitCodedError
		if errors.As(err, &coded) {
			exit(coded.Code)
			return
		}
		var ue *cmd.UsageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "Error:", err)
			if failed != nil {
				fmt.Fprintln(stderr, failed.UsageString())
			} else {
				fmt.Fprintln(stderr, rootCmd.UsageString())
			}
			exit(1)
			return
		}
		fmt.Fprintln(stderr, err)
		exit(1)
	}
}

func main() {
	repoRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: getwd:", err)
		os.Exit(1)
	}
	cfgStore := &config.FileStore{Path: ".sandman/config.yaml"}
	ghClient := github.NewCLIClient()
	commentPoster := github.NewGHCommentPoster(ghClient)
	renderer := &prompt.Engine{}
	eventLog := &events.JSONLLogger{Path: ".sandman/events.jsonl"}

	deps := cmd.Dependencies{
		BatchRunner:   batch.NewOrchestrator(ghClient, renderer, cfgStore, eventLog, batch.WithBadgeHooker(batch.NewBadgeHooker())),
		ConfigStore:   cfgStore,
		EventLog:      eventLog,
		GitHubClient:  ghClient,
		CommentPoster: commentPoster,
		Renderer:      renderer,
		IssuePicker:   &cmd.SimpleIssuePicker{},
		IsTTY:         isStdoutTTY,
		RepoRoot:      repoRoot,
	}

	rootCmd := cmd.NewRootCmd(deps)
	executeRoot(rootCmd, os.Stderr, os.Exit)
}
