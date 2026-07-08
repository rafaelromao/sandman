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

// prewarmImageFunc is the build function used by prewarmSmokeImages.
// It is a variable so tests can replace it with a fake that controls
// timing and error injection without building real containers.
var prewarmImageFunc = prewarmSmokeImage

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
		prewarmSmokeImages()
	}
	os.Exit(m.Run())
}

// prewarmSmokeImages builds every smoke variant in parallel and stores
// the resulting image tags in prebuiltSmokeImages. Failures are
// tolerated: a missing entry just means the test that needs it falls
// back to the in-test build path.
//
// Concurrency is capped at the number of variants (4) via a semaphore
// so a slow container runtime does not have to back off.
func prewarmSmokeImages() {
	var wg sync.WaitGroup
	sem := make(chan struct{}, len(smokePrewarmVariants))
	for _, v := range smokePrewarmVariants {
		v := v
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tag, err := prewarmImageFunc(v.provider, v.buildTools)
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

// TestPrewarmFailureIsolation verifies that when one variant's build fails,
// the other variants still complete and populate the prebuiltSmokeImages map.
func TestPrewarmFailureIsolation(t *testing.T) {
	realFunc := prewarmImageFunc
	defer func() { prewarmImageFunc = realFunc }()

	prewarmImageFunc = func(provider, buildTools string) (string, error) {
		if provider == "opencode" && buildTools == "elixir" {
			return "", fmt.Errorf("synthetic elixir build failure")
		}
		return "sandman-smoke-" + provider + "-" + buildTools + ":prewarm", nil
	}

	prebuiltSmokeImages = sync.Map{}

	prewarmSmokeImages()

	okVariants := []struct{ provider, buildTools string }{
		{"opencode", "generic"},
		{"opencode", "go"},
		{"opencode", "python"},
	}
	for _, v := range okVariants {
		tag := smokePrewarmLookup(v.provider, v.buildTools)
		if tag == "" {
			t.Errorf("expected prebuilt image for %s/%s after elixir failure, got empty string",
				v.provider, v.buildTools)
		}
	}

	if tag := smokePrewarmLookup("opencode", "elixir"); tag != "" {
		t.Errorf("expected no prebuilt image for failed elixir variant, got %q", tag)
	}
}

// BenchmarkPrewarmParallelism verifies that prewarmSmokeImages runs all
// variants concurrently by measuring wall-clock time. If variants ran
// sequentially, elapsed time would be the sum of all delays (100ms).
// With true parallelism, elapsed time is dominated by the slowest
// variant (40ms). The test asserts elapsed is below the sequential sum
// and above the single-variant floor, with generous margins for CI variance.
func BenchmarkPrewarmParallelism(b *testing.B) {
	realFunc := prewarmImageFunc
	defer func() { prewarmImageFunc = realFunc }()

	variantDelays := map[string]time.Duration{
		"opencode-generic": 10 * time.Millisecond,
		"opencode-go":      20 * time.Millisecond,
		"opencode-python":  30 * time.Millisecond,
		"opencode-elixir":  40 * time.Millisecond,
	}

	prewarmImageFunc = func(provider, buildTools string) (string, error) {
		delay := variantDelays[provider+"-"+buildTools]
		time.Sleep(delay)
		return "sandman-smoke-" + provider + "-" + buildTools + ":prewarm", nil
	}

	var sumDelays, maxDelay time.Duration
	for _, d := range variantDelays {
		sumDelays += d
		if d > maxDelay {
			maxDelay = d
		}
	}

	for i := 0; i < b.N; i++ {
		prebuiltSmokeImages = sync.Map{}
		start := time.Now()
		prewarmSmokeImages()
		elapsed := time.Since(start)

		maxAcceptable := time.Duration(float64(sumDelays) * 1.5)
		if elapsed >= sumDelays || elapsed < maxDelay {
			b.Errorf("prewarm elapsed=%v, want >%v && <%v (parallelism broken: got sequential or faster-than-possible)",
				elapsed, maxDelay, maxAcceptable)
		}

		b.ReportMetric(float64(elapsed)/float64(maxDelay), "ratio_to_slowest_variant")
	}
}

// TestPrewarmSmokeLookup verifies that smokePrewarmLookup returns the
// correct tag for a variant after prewarmSmokeImages populates the map.
func TestPrewarmSmokeLookup(t *testing.T) {
	realFunc := prewarmImageFunc
	defer func() { prewarmImageFunc = realFunc }()

	prewarmImageFunc = func(provider, buildTools string) (string, error) {
		return "sandman-smoke-" + provider + "-" + buildTools + ":prewarm", nil
	}

	prebuiltSmokeImages = sync.Map{}
	prewarmSmokeImages()

	expected := "sandman-smoke-opencode-generic:prewarm"
	if tag := smokePrewarmLookup("opencode", "generic"); tag != expected {
		t.Errorf("smokePrewarmLookup(opencode, generic) = %q, want %q", tag, expected)
	}
}
