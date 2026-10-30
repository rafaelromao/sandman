package batch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAgentSessionID_RequiresASessionProtocol(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
		want   string
	}{
		{name: "claude", record: `{"protocol":"claude-session/v1","provider":"claude","session_id":"abc"}`, want: "abc"},
		{name: "opencode", record: `{"protocol":"opencode-session/v1","provider":"opencode","session_id":"ses_1"}`, want: "ses_1"},
		{name: "unknown protocol shape", record: `{"protocol":"anything","provider":"x","session_id":"abc"}`},
		{name: "no provider", record: `{"protocol":"claude-session/v1","session_id":"abc"}`},
		{name: "empty id", record: `{"protocol":"claude-session/v1","provider":"claude","session_id":" "}`},
		{name: "malformed", record: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(tc.record), 0644); err != nil {
				t.Fatal(err)
			}
			got, ok := ReadAgentSessionID(dir)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("ReadAgentSessionID = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	if _, ok := ReadAgentSessionID(t.TempDir()); ok {
		t.Fatal("ReadAgentSessionID reported a session for a Run without session.json")
	}
}
