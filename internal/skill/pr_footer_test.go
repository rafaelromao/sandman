package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncInstallsBuiltWithSandmanPRFooter(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<a href="https://github.com/rafaelromao/sandman"><img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" /></a>`,
		"first non-empty line", "exactly one linked Built with Sandman logo",
		"creation, continuation, and body edits", "preserve the closing reference and existing description",
		"move an existing footer to the end rather than duplicating it", "independent of the optional repository README badge",
		"closing reference and final Built with Sandman footer are verified",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("installed publication guidance missing %q", want)
		}
	}
}
