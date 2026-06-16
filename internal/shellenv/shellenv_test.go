package shellenv

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestShellenvValidateKey covers the full accept/reject matrix of
// ValidateKey in one place.
func TestShellenvValidateKey(t *testing.T) {
	t.Run("accepts", func(t *testing.T) {
		accepts := []string{"FOO", "FOO_BAR_2", "_A", "a", "X1", "MIXED_case_42"}
		for _, key := range accepts {
			if err := ValidateKey(key); err != nil {
				t.Errorf("ValidateKey(%q) returned %v, want nil", key, err)
			}
		}
	})
	t.Run("rejects", func(t *testing.T) {
		rejects := []string{
			"",
			"FOO; rm -rf /",
			"FOO&BAR",
			"FOO|BAR",
			"FOO=BAR",
			"FOO BAR",
			"1FOO",
			"FOO-BAR",
			"FOO.BAR",
			"FOO'BAR",
		}
		for _, key := range rejects {
			t.Run(key, func(t *testing.T) {
				err := ValidateKey(key)
				if err == nil {
					t.Fatalf("ValidateKey(%q) returned nil, want error", key)
				}
				var inv *InvalidKeyError
				if !errors.As(err, &inv) {
					t.Fatalf("ValidateKey(%q) returned %T, want *InvalidKeyError", key, err)
				}
				if len(inv.Keys) != 1 || inv.Keys[0] != key {
					t.Errorf("InvalidKeyError.Keys = %v, want [%q]", inv.Keys, key)
				}
			})
		}
	})
}

// TestShellenvBuild covers the behaviour of Build in one place.
func TestShellenvBuild(t *testing.T) {
	t.Run("empty env returns cmd unchanged", func(t *testing.T) {
		cases := []map[string]string{nil, {}}
		cmd := "opencode run .sandman/task.md"
		for _, env := range cases {
			got, err := Build(env, cmd)
			if err != nil {
				t.Fatalf("Build(%v, %q) returned error: %v", env, cmd, err)
			}
			if got != cmd {
				t.Errorf("Build(%v, %q) = %q, want %q", env, cmd, got, cmd)
			}
		}
	})
	t.Run("prefixes sorted exports and runs cmd", func(t *testing.T) {
		got, err := Build(map[string]string{"A": "1", "B": "two words"}, "<cmd>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "export A=1; export B='two words'; <cmd>"
		if got != want {
			t.Errorf("Build output:\ngot:  %q\nwant: %q", got, want)
		}
	})
	t.Run("escapes single quotes in values", func(t *testing.T) {
		got, err := Build(map[string]string{"S": "it's fine"}, "echo $S")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `export S='it'"'"'s fine'; echo $S`
		if got != want {
			t.Errorf("Build output:\ngot:  %q\nwant: %q", got, want)
		}
	})
	t.Run("rejects all bad keys with a single error", func(t *testing.T) {
		env := map[string]string{
			"GOOD":     "ok",
			"FOO; rm":  "x",
			"1BAD":     "y",
			"ALSO BAD": "z",
		}
		got, err := Build(env, "echo hi")
		if err == nil {
			t.Fatalf("expected error, got output %q", got)
		}
		if got != "" {
			t.Errorf("expected empty output on error, got %q", got)
		}
		var inv *InvalidKeyError
		if !errors.As(err, &inv) {
			t.Fatalf("Build returned %T, want *InvalidKeyError", err)
		}
		wantBad := map[string]bool{"FOO; rm": true, "1BAD": true, "ALSO BAD": true}
		if len(inv.Keys) != len(wantBad) {
			t.Errorf("InvalidKeyError.Keys = %v, want exactly %v", inv.Keys, wantBad)
		}
		for _, k := range inv.Keys {
			if !wantBad[k] {
				t.Errorf("unexpected key %q in error", k)
			}
		}
	})
	t.Run("round-trips under sh -c", func(t *testing.T) {
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
			t.Errorf("shell round-trip:\ngot:  %q\nwant: %q", got, want)
		}
	})
}

// FuzzValidateKey rejects any key outside the POSIX shell-variable regex and
// ensures ValidateKey's return type is *InvalidKeyError on every failure.
func FuzzValidateKey(f *testing.F) {
	f.Add("FOO_BAR_2")
	f.Add("FOO; rm -rf /")
	f.Add("")
	f.Add("1FOO")
	f.Add("ALPHA_BETA_gamMA_3")
	f.Add("ALPHA-BETA")

	f.Fuzz(func(t *testing.T, key string) {
		err := ValidateKey(key)
		if err == nil {
			// Sanity: a key that passes ValidateKey must be acceptable to Build
			// when used on its own, and must round-trip through sh -c.
			if _, berr := Build(map[string]string{key: "v"}, "true"); berr != nil {
				t.Errorf("ValidateKey(%q) accepted but Build rejected: %v", key, berr)
			}
			return
		}
		var inv *InvalidKeyError
		if !errors.As(err, &inv) {
			t.Errorf("ValidateKey(%q) error is %T, want *InvalidKeyError", key, err)
		}
		if len(inv.Keys) != 1 || inv.Keys[0] != key {
			t.Errorf("InvalidKeyError.Keys = %v, want exactly [%q]", inv.Keys, key)
		}
	})
}

// TestShellenvQuote covers Quote's always-quoted contract: every
// non-empty value must be wrapped in single quotes and embedded
// single quotes must be escaped via the `'\”` idiom. Empty values
// also render as the empty-quote literal so callers can blindly
// interpolate the result.
func TestShellenvQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: "''"},
		{in: "main", want: "'main'"},
		{in: "sandman/42-fix-bug", want: "'sandman/42-fix-bug'"},
		{in: "two words", want: "'two words'"},
		{in: "it's fine", want: `'it'"'"'s fine'`},
		{in: "'; rm -rf /", want: `''"'"'; rm -rf /'`},
	}
	for _, tc := range cases {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
