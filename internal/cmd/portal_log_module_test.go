package cmd

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPortalLogProductionModule(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", filepath.Join(filepath.Dir(file), "portal_log_module_test.js"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("production log module tests: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
