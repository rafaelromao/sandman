package shellenv

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestShellenvValidateKey_Accepts(t *testing.T) {
	if err := ValidateKey("FOO_BAR_2"); err != nil {
		t.Fatalf("expected ValidateKey(FOO_BAR_2) to return nil, got %v", err)
	}
}

func TestShellenvValidateKey_RejectsBadKeys(t *testing.T) {
	cases := []string{
		"",
		"FOO; rm -rf /",
		"FOO&BAR",
		"FOO|BAR",
		"FOO=BAR",
		"FOO BAR",
		"1FOO",
		"FOO-BAR",
		"FOO.BAR",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			err := ValidateKey(key)
			if err == nil {
				t.Fatalf("expected ValidateKey(%q) to return an error, got nil", key)
			}
			var inv *InvalidKeyError
			if !errors.As(err, &inv) {
				t.Fatalf("expected *InvalidKeyError, got %T", err)
			}
			if len(inv.Keys) != 1 || inv.Keys[0] != key {
				t.Errorf("expected InvalidKeyError to list %q, got %v", key, inv.Keys)
			}
		})
	}
}

func TestShellenvBuild_EmptyEnvReturnsCmdUnchanged(t *testing.T) {
	cases := []map[string]string{nil, {}}
	cmd := "opencode run .sandman/task.md"
	for _, env := range cases {
		got, err := Build(env, cmd)
		if err != nil {
			t.Fatalf("Build(%v, %q) returned unexpected error: %v", env, cmd, err)
		}
		if got != cmd {
			t.Errorf("Build(%v, %q) = %q, want %q", env, cmd, got, cmd)
		}
	}
}

func TestShellenvBuild_PrefixesExportsAndRunsCmd(t *testing.T) {
	env := map[string]string{"A": "1", "B": "two words"}
	got, err := Build(env, "<cmd>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "export A=1; export B='two words'; <cmd>"
	if got != want {
		t.Errorf("Build output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestShellenvBuild_EscapesSingleQuotesInValues(t *testing.T) {
	env := map[string]string{"S": "it's fine"}
	got, err := Build(env, "echo $S")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `export S='it'"'"'s fine'; echo $S`
	if got != want {
		t.Errorf("Build output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestShellenvBuild_RejectsAllBadKeys(t *testing.T) {
	env := map[string]string{
		"GOOD":     "ok",
		"FOO; rm":  "x",
		"1BAD":     "y",
		"ALSO BAD": "z",
	}
	got, err := Build(env, "echo hi")
	if err == nil {
		t.Fatalf("expected error for bad keys, got nil (output %q)", got)
	}
	if got != "" {
		t.Errorf("expected empty output on error, got %q", got)
	}
	var inv *InvalidKeyError
	if !errors.As(err, &inv) {
		t.Fatalf("expected *InvalidKeyError, got %T: %v", err, err)
	}
	wantBad := map[string]bool{"FOO; rm": true, "1BAD": true, "ALSO BAD": true}
	if len(inv.Keys) != len(wantBad) {
		t.Errorf("expected %d bad keys, got %d (%v)", len(wantBad), len(inv.Keys), inv.Keys)
	}
	for _, k := range inv.Keys {
		if !wantBad[k] {
			t.Errorf("unexpected key in error: %q", k)
		}
	}
	for k := range wantBad {
		found := false
		for _, ik := range inv.Keys {
			if ik == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected bad key %q in error, got %v", k, inv.Keys)
		}
	}
}

func TestShellenvBuild_RoundTripsUnderShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
	env := map[string]string{
		"FOO_BAR_2": "value with spaces",
		"IT":        "it's fine",
	}
	cmd := `printf "%s|%s\n" "$FOO_BAR_2" "$IT"`
	built, err := Build(env, cmd)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	out, err := exec.Command("sh", "-c", built).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v\noutput: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := "value with spaces|it's fine"
	if got != want {
		t.Errorf("shell round-trip mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func FuzzValidateKey(f *testing.F) {
	f.Add("FOO_BAR_2")
	f.Add("FOO; rm -rf /")
	f.Add("")
	f.Add("1FOO")
	f.Add("ALPHA_BETA_gamMA_3")

	pattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	f.Fuzz(func(t *testing.T, key string) {
		err := ValidateKey(key)
		bad := err != nil
		expectBad := !pattern.MatchString(key)
		if bad != expectBad {
			t.Errorf("ValidateKey(%q): got error=%v, want error=%v (regex says valid=%v)",
				key, err, expectBad, !expectBad)
		}
		if bad {
			var inv *InvalidKeyError
			if !errors.As(err, &inv) {
				t.Errorf("ValidateKey(%q) error is %T, want *InvalidKeyError", key, err)
			}
		}
	})
}
