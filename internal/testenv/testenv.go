// Package testenv provides shared helpers for parsing the env vars that
// gate sandman test suites. It is consumed by smoke tests, prflow e2e
// tests, and the batch orchestrator end-to-end test to decide which
// providers and scenarios should run, and to parameterize the model
// each agent-driven test targets.
//
// Canonical env vars:
//
//   - SANDMAN_TEST_PROVIDERS      — comma list of provider names, "all", or "*".
//     Drives provider allowlists in smoke and e2e tests. The known names
//     are passed in by the caller (e.g. the smoke or prflow case lists).
//   - SANDMAN_E2E_GATES           — comma list of scenario names, "all", or "*".
//     Stable scenario identifiers are E2EScenarioBatch and
//     E2EScenarioContinueMulti.
//   - SANDMAN_TEST_MODEL_<AGENT>  — one var per supported agent (e.g.
//     SANDMAN_TEST_MODEL_OPENCODE, SANDMAN_TEST_MODEL_PI). Overrides the
//     model the smoke and e2e tests target for that agent. When unset,
//     the test falls back to the literal model baked into the test case.
//
// When the gate vars are unset, helpers return the skip-friendly default
// (nil allowlist / false gate) and tests skip themselves. The model
// override is a pure value substitution with no skip semantics.
package testenv

import (
	"fmt"
	"os"
	"strings"
)

// E2E scenario identifiers (stable across versions).
const (
	E2EScenarioBatch            = "batch"
	E2EScenarioContinueMulti    = "continue_multi"
	E2EScenarioOpencodeSubagent = "opencode_subagent"
	E2EScenarioBadge            = "badge"
	E2EScenarioPathlen          = "pathlen"
)

// Canonical env var names.
const (
	CanonicalE2EGatesEnvVar     = "SANDMAN_E2E_GATES"
	CanonicalProviderListEnvVar = "SANDMAN_TEST_PROVIDERS"
)

// TestModelEnvVar returns the canonical env var name that overrides the
// model the smoke and e2e tests use for the given agent. The returned
// name follows the SANDMAN_TEST_MODEL_<AGENT> convention and is always
// uppercased regardless of the agent casing the caller passes in.
func TestModelEnvVar(agent string) string {
	return "SANDMAN_TEST_MODEL_" + strings.ToUpper(strings.TrimSpace(agent))
}

// ResolveTestModel returns the model string the smoke and e2e tests
// should target for the given agent. If the canonical per-agent env
// var (SANDMAN_TEST_MODEL_<AGENT>) is set to a non-empty value, that
// value (trimmed) is returned. Otherwise the supplied defaultModel is
// returned unchanged. The helper is the only place that knows about
// the env var convention, keeping the call sites declarative.
func ResolveTestModel(agent, defaultModel string) string {
	override := strings.TrimSpace(os.Getenv(TestModelEnvVar(agent)))
	if override == "" {
		return defaultModel
	}
	return override
}

// allE2EScenarios is the canonical list of stable scenario identifiers
// accepted by SANDMAN_E2E_GATES. Adding a new scenario requires editing
// this list and exporting a new E2EScenario* constant.
var allE2EScenarios = []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent, E2EScenarioBadge, E2EScenarioPathlen}

// ParseList parses a comma-separated allowlist. Semantics:
//   - empty/whitespace raw returns nil (no filter)
//   - "all" or "*" returns a map with every name in `known` set to true
//   - comma list returns an explicit allowlist with strict validation
//     against `known`; unknown names produce an error. Empty entries are
//     skipped. Surrounding whitespace around each entry is trimmed.
//
// `kind` is used only for error messages (e.g. "provider", "scenario").
func ParseList(raw string, known []string, kind string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw == "all" || raw == "*" {
		allowed := make(map[string]bool, len(known))
		for _, name := range known {
			allowed[name] = true
		}
		return allowed, nil
	}
	knownSet := make(map[string]bool, len(known))
	for _, name := range known {
		knownSet[name] = true
	}
	allowed := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !knownSet[name] {
			if kind == "" {
				return nil, fmt.Errorf("unknown entry %q", name)
			}
			return nil, fmt.Errorf("unknown %s %q", kind, name)
		}
		allowed[name] = true
	}
	return allowed, nil
}

// ResolveProviderAllowlist resolves a provider allowlist from the
// canonical env var SANDMAN_TEST_PROVIDERS. Returns nil if it is unset.
func ResolveProviderAllowlist(known []string) (map[string]bool, error) {
	if raw := strings.TrimSpace(os.Getenv(CanonicalProviderListEnvVar)); raw != "" {
		return ParseList(raw, known, "provider")
	}
	return nil, nil
}

// E2EGateListAllowed reports whether `scenario` is enabled by the parsed
// canonical gate list. `raw` is the value of the canonical env var. An
// empty or invalid canonical value disables every scenario. Returns
// false for invalid canonical values.
func E2EGateListAllowed(scenario, raw string, known []string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	allowed, err := ParseList(raw, known, "scenario")
	if err != nil {
		return false
	}
	return allowed[scenario]
}

// E2EGateAllowed is a thin convenience wrapper around E2EGateListAllowed
// that reads the canonical e2e gate env var from the environment. Use it
// at test entry points:
//
//	if !testenv.E2EGateAllowed(testenv.E2EScenarioBatch) {
//	    t.Skip(...)
//	}
func E2EGateAllowed(scenario string) bool {
	return E2EGateListAllowed(scenario, os.Getenv(CanonicalE2EGatesEnvVar), allE2EScenarios)
}
