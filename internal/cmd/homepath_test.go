//go:build smoke || e2e

package cmd

import (
	"path/filepath"
	"strings"
)

func homePath(home, rel string) string {
	rel = strings.TrimPrefix(rel, "~")
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return filepath.Join(home, rel)
}
