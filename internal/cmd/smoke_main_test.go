//go:build smoke

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/scaffold"
)

// prebuiltSmokeImages maps a "provider-buildTools" key to a prebuilt image
// tag. Populated by TestMain when pre-warm is enabled. The map is read by
// preflightSmokeImage so per-test runs reuse a single image instead of
// building a fresh one for every (provider, buildTools) combination.
var prebuiltSmokeImages sync.Map

// prewarmOnce guards the one-shot pre-warm so TestMain does not rebuild
// the images if it is invoked more than once (e.g. when re-running the
// package from a sub-agent).
var prewarmOnce sync.Once

// smokePrewarmVariants enumerates the (provider, buildTools) pairs whose
// images the pre-warm builds. These are the cross-product of the
// smoke provider (opencode) and the buildTools variants
// (generic, go, python) that TestSmoke_RealAgentCLIs_* exercises via
// buildTools overrides on smokeProviderCases.
var smokePrewarmVariants = []smokePrewarmVariant{
	{provider: "opencode", buildTools: "generic"},
	{provider: "opencode", buildTools: "go"},
	{provider: "opencode", buildTools: "python"},
	{provider: "opencode", buildTools: "elixir"},
}

type smokePrewarmVariant struct {
	provider   string
	buildTools string
}

// TestMain runs the pre-warm phase before any test executes, when the
// smoke build tag is set. The pre-warm scaffolds each (provider,
// buildTools) image once and stores the tag in a package-level map so
// the per-test preflight can reuse it instead of paying the cold build
// cost on every test invocation.
//
// Set SANDMAN_SMOKE_PREFETCH=0 to skip the pre-warm and fall back to the
// per-test in-test build (useful when iterating on the Dockerfile or
// when you want every test to be hermetic).
func TestMain(m *testing.M) {
	applySmokeModelOverrides()
	if os.Getenv("SANDMAN_SMOKE_PREFETCH") != "0" {
		prewarmOnce.Do(prewarmSmokeImages)
	}
	os.Exit(m.Run())
}

// prewarmSmokeImages builds every smoke variant in parallel and stores
// the resulting image tags in prebuiltSmokeImages. Failures are
// tolerated: a missing entry just means the test that needs it falls
// back to the in-test build path.
func prewarmSmokeImages() {
	var wg sync.WaitGroup
	for _, v := range smokePrewarmVariants {
		v := v
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag, err := prewarmSmokeImage(v.provider, v.buildTools)
			if err != nil || tag == "" {
				return
			}
			prebuiltSmokeImages.Store(v.provider+"-"+v.buildTools, tag)
		}()
	}
	wg.Wait()
}

// prewarmSmokeImage scaffolds a throwaway repo for the given
// (provider, buildTools) pair, appends smoke-only Dockerfile lines,
// and runs `podman build`. Returns the image tag on success, "" on
// failure (the caller treats failure as "no prebuilt image; fall
// back to the per-test build").
func prewarmSmokeImage(provider, buildTools string) (string, error) {
	repoDir, err := os.MkdirTemp("", "sandman-smoke-prewarm-")
	if err != nil {
		return "", fmt.Errorf("create prewarm repo dir: %w", err)
	}
	defer os.RemoveAll(repoDir)

	s := &scaffold.Scaffolder{}
	if err := s.Scaffold(repoDir, scaffold.Options{BuildTools: buildTools, Agent: provider}, smokePrompter{}); err != nil {
		return "", fmt.Errorf("scaffold prewarm: %w", err)
	}
	if err := addSmokeDockerDeps(repoDir, provider, buildTools); err != nil {
		return "", fmt.Errorf("add smoke docker deps: %w", err)
	}

	runtime, err := resolvePrewarmRuntime()
	if err != nil {
		return "", fmt.Errorf("resolve runtime: %w", err)
	}
	tag := fmt.Sprintf("sandman-smoke-%s-%s:prewarm", provider, buildTools)
	cmd := exec.Command(runtime, "build", "-t", tag, "-f", filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("prewarm build: %w: %s", err, out)
	}
	return tag, nil
}

// resolvePrewarmRuntime returns the container runtime binary to use for
// pre-warm builds. Mirrors sandbox.ResolveRuntime but is duplicated
// here to keep this file self-contained under the smoke build tag.
func resolvePrewarmRuntime() (string, error) {
	for _, candidate := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no container runtime (podman or docker) found in PATH")
}

// smokePrewarmLookup returns the prebuilt image tag for the given
// (provider, buildTools) pair, or "" if no prebuilt image is
// available. Used by preflightSmokeImage to short-circuit the
// per-test build.
func smokePrewarmLookup(provider, buildTools string) string {
	if v, ok := prebuiltSmokeImages.Load(provider + "-" + buildTools); ok {
		if tag, ok := v.(string); ok {
			return tag
		}
	}
	return ""
}

// avoid an unused-import warning when this file is the only consumer
// of time.Now in the smoke build.
var _ = time.Now
