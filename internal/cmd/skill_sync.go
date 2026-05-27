package cmd

import (
	"io"
	"os"

	"github.com/rafaelromao/sandman/internal/skill"
	"github.com/spf13/cobra"
)

var syncSandmanSkill = skill.Sync

func sandmanSkillSyncOptions(cmd *cobra.Command, reviewCommand string) (skill.SyncOptions, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return skill.SyncOptions{}, err
	}
	return skill.SyncOptions{
		HomeDir:       homeDir,
		ReviewCommand: reviewCommand,
		In:            cmd.InOrStdin(),
		Out:           cmd.OutOrStdout(),
		Interactive:   isTerminalReader(cmd.InOrStdin()),
	}, nil
}

func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
