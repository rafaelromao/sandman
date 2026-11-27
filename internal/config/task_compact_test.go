package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_TaskCompactBytes(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       int
		err        string
	}{
		{name: "unset uses the default", yaml: "", want: DefaultTaskCompactBytes},
		{name: "zero disables", yaml: "task_compact_bytes: 0\n", want: 0},
		{name: "fraction below one", yaml: "task_compact_bytes: 0.5\n", err: "task_compact_bytes must be an integer"},
		{name: "fraction above minimum", yaml: "task_compact_bytes: 32768.5\n", err: "task_compact_bytes must be an integer"},
		{name: "explicit", yaml: "task_compact_bytes: 65536\n", want: 65536},
		{name: "minimum", yaml: "task_compact_bytes: 32768\n", want: 32768},
		{name: "below the minimum", yaml: "task_compact_bytes: 32767\n", err: "task_compact_bytes must be 0 or at least 32768"},
		{name: "negative", yaml: "task_compact_bytes: -1\n", err: "task_compact_bytes must be 0 or at least 32768"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfigText(t, "agent: opencode\n"+tc.yaml)

			if tc.err != "" {
				requireErrorMentions(t, err, tc.err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.EffectiveTaskCompactBytes(); got != tc.want {
				t.Fatalf("EffectiveTaskCompactBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestConfigSet_TaskCompactBytes(t *testing.T) {
	cfg := &Config{}
	if got, err := cfg.GetValue("task_compact_bytes"); err != nil || got != "49152" {
		t.Fatalf("default = %q, %v", got, err)
	}
	for _, bad := range []string{"32767", "-5", "big"} {
		if err := cfg.SetValue("task_compact_bytes", bad); err == nil {
			t.Errorf("set %q was accepted", bad)
		}
	}
	if err := cfg.SetValue("task_compact_bytes", "0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := cfg.GetValue("task_compact_bytes"); got != "0" {
		t.Fatalf("after set 0 = %q", got)
	}
	found := false
	for _, key := range SupportedKeys() {
		found = found || key == "task_compact_bytes"
	}
	if !found {
		t.Fatal("task_compact_bytes is not a supported key")
	}
}

// Disabling compaction must survive saving: an unset value and 0 differ.
func TestConfigSave_KeepsTaskCompactBytesZeroDistinctFromUnset(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want int
	}{{"", DefaultTaskCompactBytes}, {"0", 0}, {"40000", 40000}} {
		cfg, err := loadConfigText(t, "agent: opencode\n")
		if err != nil {
			t.Fatal(err)
		}
		if tc.set != "" {
			if err := cfg.SetValue("task_compact_bytes", tc.set); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		reloaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := reloaded.EffectiveTaskCompactBytes(); got != tc.want {
			t.Errorf("set %q: reloaded %d, want %d", tc.set, got, tc.want)
		}
		if tc.set == "" {
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "task_compact_bytes") {
				t.Errorf("an unset value was written:\n%s", raw)
			}
		}
	}
}
