package testenv

import (
	"fmt"
	"os"
	"strings"
)

// E2E scenario identifiers (stable across versions).
const (
	E2EScenarioBatch         = "batch"
	E2EScenarioContinueMulti = "continue_multi"
)

// CanonicalE2EGatesEnvVar is the canonical env var that gates e2e scenarios.
const CanonicalE2EGatesEnvVar = "SANDMAN_E2E_GATES"

// CanonicalProviderListEnvVar is the canonical env var that gates provider allowlists.
const CanonicalProviderListEnvVar = "SANDMAN_TEST_PROVIDERS"

// ParseList parses a comma-separated allowlist. Semantics:
//   - empty/whitespace raw returns nil (no filter)
//   - "all" or "*" returns a map with every name in `known` set to true
//   - comma list returns an explicit allowlist with strict validation
//     against `known`; unknown names produce an error. Empty entries are
//     skipped. Surrounding whitespace around each entry is trimmed.
func ParseList(raw string, known []string) (map[string]bool, error) {
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
			return nil, fmt.Errorf("unknown entry %q", name)
		}
		allowed[name] = true
	}
	return allowed, nil
}

// ResolveProviderAllowlist resolves a provider allowlist from the canonical
// env var (SANDMAN_TEST_PROVIDERS) with a fallback to a legacy env var
// (e.g. SANDMAN_SMOKE_PROVIDERS, SANDMAN_E2E_PROVIDERS). The canonical var
// takes precedence when set; the legacy var is consulted only when the
// canonical var is empty. Returns nil if neither var is set.
func ResolveProviderAllowlist(legacyEnvVar string, known []string) (map[string]bool, error) {
	if raw := strings.TrimSpace(os.Getenv(CanonicalProviderListEnvVar)); raw != "" {
		return ParseList(raw, known)
	}
	if raw := strings.TrimSpace(os.Getenv(legacyEnvVar)); raw != "" {
		return ParseList(raw, known)
	}
	return nil, nil
}

// E2EGateListAllowed reports whether `scenario` is enabled by the parsed
// canonical gate list. `raw` is the value of the canonical env var. When
// `raw` is empty, the function falls back to a presence check on
// `legacyEnvVar`. The canonical value always wins: if it is set, the
// legacy var is ignored. Returns false for invalid canonical values.
func E2EGateListAllowed(scenario, raw, legacyEnvVar string, known []string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return legacyEnvVar != "" && os.Getenv(legacyEnvVar) != ""
	}
	allowed, err := ParseList(raw, known)
	if err != nil {
		return false
	}
	return allowed[scenario]
}

// E2EGateAllowed is a thin convenience wrapper around E2EGateListAllowed
// that reads the canonical e2e gate env var from the environment. Use it
// at test entry points: `if !testenv.E2EGateAllowed(testenv.E2EScenarioBatch, "SANDMAN_E2E") { t.Skip(...) }`.
func E2EGateAllowed(scenario, legacyEnvVar string) bool {
	return E2EGateListAllowed(scenario, os.Getenv(CanonicalE2EGatesEnvVar), legacyEnvVar, []string{E2EScenarioBatch, E2EScenarioContinueMulti})
}
