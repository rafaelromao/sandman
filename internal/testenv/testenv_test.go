package testenv

import (
	"reflect"
	"testing"
)

func TestParseList_EmptyReturnsNil(t *testing.T) {
	allowed, err := ParseList("", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist for empty input, got %#v", allowed)
	}
}

func TestParseList_WhitespaceOnlyReturnsNil(t *testing.T) {
	allowed, err := ParseList("   ", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist for whitespace input, got %#v", allowed)
	}
}

func TestParseList_AllReturnsKnown(t *testing.T) {
	allowed, err := ParseList("all", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_StarReturnsKnown(t *testing.T) {
	allowed, err := ParseList("*", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_CommaListReturnsExplicit(t *testing.T) {
	allowed, err := ParseList("opencode,pi", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_TrimsWhitespaceAroundEntries(t *testing.T) {
	allowed, err := ParseList("  opencode , pi  ", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_EmptyEntriesAreSkipped(t *testing.T) {
	allowed, err := ParseList(",,opencode,,,pi,,", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_UnknownNameReturnsError(t *testing.T) {
	_, err := ParseList("opencode,claude", []string{"opencode", "pi"})
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
}

func TestParseList_SingleUnknownNameReturnsError(t *testing.T) {
	_, err := ParseList("claude", []string{"opencode", "pi"})
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
}

func TestResolveProviderAllowlist_NeitherSetReturnsNil(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "")
	t.Setenv("SANDMAN_LEGACY_PROVIDERS", "")
	allowed, err := ResolveProviderAllowlist("SANDMAN_LEGACY_PROVIDERS", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist, got %#v", allowed)
	}
}

func TestResolveProviderAllowlist_CanonicalWinsOverLegacy(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "opencode")
	t.Setenv("SANDMAN_LEGACY_PROVIDERS", "pi")
	allowed, err := ResolveProviderAllowlist("SANDMAN_LEGACY_PROVIDERS", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected canonical to win: want %#v, got %#v", want, allowed)
	}
}

func TestResolveProviderAllowlist_LegacyUsedWhenCanonicalUnset(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "")
	t.Setenv("SANDMAN_LEGACY_PROVIDERS", "opencode,pi")
	allowed, err := ResolveProviderAllowlist("SANDMAN_LEGACY_PROVIDERS", []string{"opencode", "pi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "pi": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected legacy fallback: want %#v, got %#v", want, allowed)
	}
}

func TestE2EGateListAllowed_AllEnablesEverything(t *testing.T) {
	got := E2EGateListAllowed(E2EScenarioBatch, "all", "", []string{E2EScenarioBatch, E2EScenarioContinueMulti})
	if !got {
		t.Fatal("expected batch enabled when raw=\"all\"")
	}
	got = E2EGateListAllowed(E2EScenarioContinueMulti, "all", "", []string{E2EScenarioBatch, E2EScenarioContinueMulti})
	if !got {
		t.Fatal("expected continue_multi enabled when raw=\"all\"")
	}
}

func TestE2EGateListAllowed_StarEnablesEverything(t *testing.T) {
	got := E2EGateListAllowed(E2EScenarioBatch, "*", "", []string{E2EScenarioBatch, E2EScenarioContinueMulti})
	if !got {
		t.Fatal("expected batch enabled when raw=\"*\"")
	}
}

func TestE2EGateListAllowed_SingleScenarioEnablesOnlyThat(t *testing.T) {
	if !E2EGateListAllowed(E2EScenarioBatch, E2EScenarioBatch, "", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch enabled when raw=\"batch\"")
	}
	if E2EGateListAllowed(E2EScenarioContinueMulti, E2EScenarioBatch, "", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected continue_multi disabled when raw=\"batch\"")
	}
}

func TestE2EGateListAllowed_CommaListEnablesListedScenarios(t *testing.T) {
	raw := E2EScenarioBatch + "," + E2EScenarioContinueMulti
	if !E2EGateListAllowed(E2EScenarioBatch, raw, "", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch enabled")
	}
	if !E2EGateListAllowed(E2EScenarioContinueMulti, raw, "", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected continue_multi enabled")
	}
}

func TestE2EGateListAllowed_LegacyFallbackWhenCanonicalUnset(t *testing.T) {
	t.Setenv("SANDMAN_LEGACY_BATCH", "1")
	if !E2EGateListAllowed(E2EScenarioBatch, "", "SANDMAN_LEGACY_BATCH", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch enabled via legacy var fallback")
	}
	t.Setenv("SANDMAN_LEGACY_BATCH", "")
	if E2EGateListAllowed(E2EScenarioBatch, "", "SANDMAN_LEGACY_BATCH", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch disabled when neither var set")
	}
}

func TestE2EGateListAllowed_CanonicalExcludesScenarioOverridesLegacy(t *testing.T) {
	t.Setenv("SANDMAN_LEGACY_CONTINUE", "1")
	if E2EGateListAllowed(E2EScenarioContinueMulti, E2EScenarioBatch, "SANDMAN_LEGACY_CONTINUE", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected canonical to take precedence: continue_multi should be disabled when raw=batch even if legacy is set")
	}
	if !E2EGateListAllowed(E2EScenarioBatch, E2EScenarioBatch, "SANDMAN_LEGACY_CONTINUE", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch enabled by canonical raw")
	}
}

func TestE2EGateListAllowed_InvalidRawDisables(t *testing.T) {
	if E2EGateListAllowed(E2EScenarioBatch, "claude", "", []string{E2EScenarioBatch, E2EScenarioContinueMulti}) {
		t.Fatal("expected batch disabled when raw contains unknown scenario")
	}
}

func TestE2EGateAllowed_ReadsCanonicalEnv(t *testing.T) {
	t.Setenv("SANDMAN_E2E_GATES", E2EScenarioBatch)
	if !E2EGateAllowed(E2EScenarioBatch, "SANDMAN_LEGACY_BATCH") {
		t.Fatal("expected batch enabled via canonical env")
	}
	if E2EGateAllowed(E2EScenarioContinueMulti, "SANDMAN_LEGACY_CONTINUE") {
		t.Fatal("expected continue_multi disabled when not listed in canonical env")
	}
}

func TestE2EGateAllowed_ReadsLegacyEnvWhenCanonicalUnset(t *testing.T) {
	t.Setenv("SANDMAN_E2E_GATES", "")
	t.Setenv("SANDMAN_LEGACY_BATCH", "1")
	if !E2EGateAllowed(E2EScenarioBatch, "SANDMAN_LEGACY_BATCH") {
		t.Fatal("expected batch enabled via legacy env fallback")
	}
}

func TestE2EGateAllowed_DisabledWhenNeitherSet(t *testing.T) {
	t.Setenv("SANDMAN_E2E_GATES", "")
	t.Setenv("SANDMAN_LEGACY_BATCH", "")
	if E2EGateAllowed(E2EScenarioBatch, "SANDMAN_LEGACY_BATCH") {
		t.Fatal("expected batch disabled when neither var set")
	}
}
