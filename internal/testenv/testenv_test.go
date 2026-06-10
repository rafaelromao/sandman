package testenv

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseList_EmptyReturnsNil(t *testing.T) {
	allowed, err := ParseList("", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist for empty input, got %#v", allowed)
	}
}

func TestParseList_WhitespaceOnlyReturnsNil(t *testing.T) {
	allowed, err := ParseList("   ", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist for whitespace input, got %#v", allowed)
	}
}

func TestParseList_AllReturnsKnown(t *testing.T) {
	allowed, err := ParseList("all", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_StarReturnsKnown(t *testing.T) {
	allowed, err := ParseList("*", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_CommaListReturnsExplicit(t *testing.T) {
	allowed, err := ParseList("opencode,claude", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_TrimsWhitespaceAroundEntries(t *testing.T) {
	allowed, err := ParseList("  opencode , claude  ", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_EmptyEntriesAreSkipped(t *testing.T) {
	allowed, err := ParseList(",,opencode,,,claude,,", []string{"opencode", "claude"}, "provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected %#v, got %#v", want, allowed)
	}
}

func TestParseList_UnknownNameReturnsError(t *testing.T) {
	_, err := ParseList("opencode,claude", []string{"opencode", "custom"}, "provider")
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Fatalf("expected error to mention kind, got %v", err)
	}
}

func TestParseList_SingleUnknownNameReturnsError(t *testing.T) {
	_, err := ParseList("claude", []string{"opencode", "custom"}, "provider")
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
}

func TestParseList_EmptyKindUsesGenericError(t *testing.T) {
	_, err := ParseList("claude", []string{"opencode", "custom"}, "")
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
	if strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected generic error when kind is empty, got %v", err)
	}
}

func TestResolveProviderAllowlist_NeitherSetReturnsNil(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "")
	allowed, err := ResolveProviderAllowlist([]string{"opencode", "claude"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed != nil {
		t.Fatalf("expected nil allowlist, got %#v", allowed)
	}
}

func TestResolveProviderAllowlist_CanonicalReturnedAsAllowlist(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "opencode")
	allowed, err := ResolveProviderAllowlist([]string{"opencode", "claude"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected canonical allowlist: want %#v, got %#v", want, allowed)
	}
}

func TestResolveProviderAllowlist_AllExpandsToKnown(t *testing.T) {
	t.Setenv("SANDMAN_TEST_PROVIDERS", "all")
	allowed, err := ResolveProviderAllowlist([]string{"opencode", "claude"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"opencode": true, "claude": true}
	if !reflect.DeepEqual(allowed, want) {
		t.Fatalf("expected canonical to expand all: want %#v, got %#v", want, allowed)
	}
}

func TestE2EGateListAllowed_AllEnablesEverything(t *testing.T) {
	got := E2EGateListAllowed(E2EScenarioBatch, "all", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent})
	if !got {
		t.Fatal("expected batch enabled when raw=\"all\"")
	}
	got = E2EGateListAllowed(E2EScenarioContinueMulti, "all", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent})
	if !got {
		t.Fatal("expected continue_multi enabled when raw=\"all\"")
	}
	got = E2EGateListAllowed(E2EScenarioOpencodeSubagent, "all", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent})
	if !got {
		t.Fatal("expected opencode_subagent enabled when raw=\"all\"")
	}
}

func TestE2EGateListAllowed_StarEnablesEverything(t *testing.T) {
	got := E2EGateListAllowed(E2EScenarioBatch, "*", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent})
	if !got {
		t.Fatal("expected batch enabled when raw=\"*\"")
	}
}

func TestE2EGateListAllowed_SingleScenarioEnablesOnlyThat(t *testing.T) {
	if !E2EGateListAllowed(E2EScenarioBatch, E2EScenarioBatch, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected batch enabled when raw=\"batch\"")
	}
	if E2EGateListAllowed(E2EScenarioContinueMulti, E2EScenarioBatch, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected continue_multi disabled when raw=\"batch\"")
	}
	if E2EGateListAllowed(E2EScenarioOpencodeSubagent, E2EScenarioBatch, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected opencode_subagent disabled when raw=\"batch\"")
	}
}

func TestE2EGateListAllowed_CommaListEnablesListedScenarios(t *testing.T) {
	raw := E2EScenarioBatch + "," + E2EScenarioContinueMulti
	if !E2EGateListAllowed(E2EScenarioBatch, raw, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected batch enabled")
	}
	if !E2EGateListAllowed(E2EScenarioContinueMulti, raw, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected continue_multi enabled")
	}
	if E2EGateListAllowed(E2EScenarioOpencodeSubagent, raw, []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected opencode_subagent disabled when not listed")
	}
}

func TestE2EGateListAllowed_EmptyRawDisables(t *testing.T) {
	if E2EGateListAllowed(E2EScenarioBatch, "", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected batch disabled when raw is empty")
	}
	if E2EGateListAllowed(E2EScenarioContinueMulti, "  ", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected continue_multi disabled when raw is whitespace")
	}
}

func TestE2EGateListAllowed_InvalidRawDisables(t *testing.T) {
	if E2EGateListAllowed(E2EScenarioBatch, "claude", []string{E2EScenarioBatch, E2EScenarioContinueMulti, E2EScenarioOpencodeSubagent}) {
		t.Fatal("expected batch disabled when raw contains unknown scenario")
	}
}

func TestE2EGateAllowed_ReadsCanonicalEnv(t *testing.T) {
	t.Setenv("SANDMAN_E2E_GATES", E2EScenarioBatch)
	if !E2EGateAllowed(E2EScenarioBatch) {
		t.Fatal("expected batch enabled via canonical env")
	}
	if E2EGateAllowed(E2EScenarioContinueMulti) {
		t.Fatal("expected continue_multi disabled when not listed in canonical env")
	}
}

func TestE2EGateAllowed_DisabledWhenCanonicalUnset(t *testing.T) {
	t.Setenv("SANDMAN_E2E_GATES", "")
	if E2EGateAllowed(E2EScenarioBatch) {
		t.Fatal("expected batch disabled when canonical env unset")
	}
}

func TestTestModelEnvVar_UppercasesAgent(t *testing.T) {
	if got, want := TestModelEnvVar("opencode"), "SANDMAN_TEST_MODEL_OPENCODE"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if got, want := TestModelEnvVar("claude"), "SANDMAN_TEST_MODEL_CLAUDE"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if got, want := TestModelEnvVar("OpenCode"), "SANDMAN_TEST_MODEL_OPENCODE"; got != want {
		t.Fatalf("expected mixed-case input to upper-case, got %q", got)
	}
	if got, want := TestModelEnvVar("  claude  "), "SANDMAN_TEST_MODEL_CLAUDE"; got != want {
		t.Fatalf("expected whitespace-trimmed upper-case, got %q", got)
	}
}

func TestResolveTestModel_UnsetReturnsDefault(t *testing.T) {
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "")
	if got := ResolveTestModel("opencode", "opencode/big-pickle"); got != "opencode/big-pickle" {
		t.Fatalf("expected default, got %q", got)
	}
}

func TestResolveTestModel_SetReturnsOverride(t *testing.T) {
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode/some-other-model")
	if got := ResolveTestModel("opencode", "opencode/big-pickle"); got != "opencode/some-other-model" {
		t.Fatalf("expected override, got %q", got)
	}
}

func TestResolveTestModel_TrimsSurroundingWhitespace(t *testing.T) {
	t.Setenv("SANDMAN_TEST_MODEL_CLAUDE", "   kilo/some-model  ")
	if got := ResolveTestModel("claude", "kilo/kilo-auto/free"); got != "kilo/some-model" {
		t.Fatalf("expected trimmed override, got %q", got)
	}
}

func TestResolveTestModel_EmptyOverrideReturnsDefault(t *testing.T) {
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "   ")
	if got := ResolveTestModel("opencode", "opencode/big-pickle"); got != "opencode/big-pickle" {
		t.Fatalf("expected default when override is whitespace, got %q", got)
	}
}

func TestResolveTestModel_AgentScoped(t *testing.T) {
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode/x")
	if got := ResolveTestModel("claude", "kilo/kilo-auto/free"); got != "kilo/kilo-auto/free" {
		t.Fatalf("expected claude default unaffected by opencode env, got %q", got)
	}
}
