package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/sandbox"
	"gopkg.in/yaml.v3"
)

// prepareSnapshotParent returns the parent directory under which the
// container config snapshot should be stored, plus a cleanup that
// removes the parent when no run owns it. When runDir is set, the
// caller is expected to remove runDir at end of run, so this cleanup is
// a no-op for the parent itself but still removes the `config/` subtree
// (handled by sandbox.ResolveConfigMounts cleanup).
func prepareSnapshotParent(runDir string) (string, func(), error) {
	if runDir != "" {
		if err := os.MkdirAll(runDir, 0755); err != nil {
			return "", nil, fmt.Errorf("prepare run dir for config snapshot: %w", err)
		}
		return runDir, func() {}, nil
	}
	tmpDir, err := os.MkdirTemp("", "sandman-config-*")
	if err != nil {
		return "", nil, fmt.Errorf("create config temp dir: %w", err)
	}
	return tmpDir, func() { _ = os.RemoveAll(tmpDir) }, nil
}

// PrepareContainerConfigMounts resolves git, gh, ssh, and agent config paths
// into ConfigMounts copied under `<runDir>/config/` so container runs do not
// bind host paths directly and can safely follow symlinked config trees.
// When runDir is empty (callers without a run-owned parent), a temp dir is
// created and the snapshot is removed by the returned cleanup.
//
// Paths in opts.AgentConfigExcludes are skipped during the snapshot copy.
// Paths in opts.LiveMounts are bind-mounted directly into the container so
// host-side state remains accessible after the container run completes;
// LiveMounts are also implicitly excluded from the snapshot — without that
// union, the snapshot copy of a live-mounted file would shadow the live
// bind mount and the host file would be neither read nor written.
//
// lookupGHToken is the explicit port used to resolve the host GitHub auth
// token when hydrating the copied gh hosts.yml; production callers pass a
// function that shells out to `gh auth token`, tests pass a fake. Takes
// a context so the spawned `gh auth token` invocation honours the
// caller's cancellation (issue #1780).
func PrepareContainerConfigMounts(ctx context.Context, repoPath, runDir string, opts *sandbox.StartOptions, lookupGHToken func(context.Context) (string, error)) (func(), error) {
	dirs := append([]string(nil), opts.AgentConfigDirs...)
	files := append([]string(nil), opts.AgentConfigFiles...)
	excludes := append([]string(nil), opts.AgentConfigExcludes...)
	// A LiveMount is by definition not in the snapshot: union it into the
	// exclude set so the dir walker skips the file before it ever lands in
	// the snapshot tree. This makes the LiveMount the single source of truth
	// for the container's view of that path.
	excludes = append(excludes, opts.LiveMounts...)

	convertedGitConfig := false
	if opts.GitConfigPath != "" {
		files = append(files, opts.GitConfigPath)
		convertedGitConfig = true
	}

	convertedSSH := false
	if opts.SSH {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir for ssh config: %w", err)
		}
		sshDir := filepath.Join(home, ".ssh")
		if _, err := os.Stat(sshDir); err == nil {
			dirs = append(dirs, sshDir)
			convertedSSH = true
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat ssh config dir %q: %w", sshDir, err)
		}
	}

	if len(dirs) == 0 && len(files) == 0 {
		return func() {}, nil
	}

	snapshotParent, snapshotCleanup, err := prepareSnapshotParent(runDir)
	if err != nil {
		return nil, err
	}

	mounts, releaseMounts, err := sandbox.ResolveConfigMounts(snapshotParent, dirs, files, excludes)
	if err != nil {
		snapshotCleanup()
		return nil, fmt.Errorf("resolve config mounts: %w", err)
	}
	releaseSnapshot := snapshotCleanup
	mounts = addSSHMountAliases(mounts)

	cleanup := func() {
		releaseMounts()
		releaseSnapshot()
	}

	if convertedGitConfig {
		if err := rewriteGitConfigMount(repoPath, mounts); err != nil {
			cleanup()
			return nil, err
		}
		opts.GitConfigPath = ""
	}

	if convertedSSH {
		opts.SSH = false
	}

	if err := hydrateGHConfigMount(ctx, mounts, lookupGHToken); err != nil {
		cleanup()
		return nil, err
	}

	mounts = appendLiveMounts(mounts, opts.LiveMounts)

	opts.ConfigMounts = mounts
	return cleanup, nil
}

// appendLiveMounts adds a ConfigMount for each existing live-mount path so
// that the host path is bind-mounted directly into the container, shadowing
// the surrounding snapshot mount. Missing paths are silently skipped so an
// agent run does not fail when an optional file (for example the SQLite WAL
// sibling files) is absent.
func appendLiveMounts(mounts []sandbox.ConfigMount, liveMounts []string) []sandbox.ConfigMount {
	for _, path := range liveMounts {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		mounts = append(mounts, sandbox.NewLiveConfigMount(path))
	}
	return mounts
}

func addSSHMountAliases(mounts []sandbox.ConfigMount) []sandbox.ConfigMount {
	seenTargets := make(map[string]bool, len(mounts))
	for _, mount := range mounts {
		seenTargets[mount.Target] = true
	}

	for _, mount := range mounts {
		if mount.Target != "/.ssh" || seenTargets["/root/.ssh"] {
			continue
		}
		// OpenSSH expands ~/.ssh from the passwd home directory (/root in our
		// podman containers), not from HOME=/. Mirror the copied ssh dir there
		// so git/ssh can find known_hosts and identity files.
		mounts = append(mounts, sandbox.ConfigMount{Source: mount.Source, Target: "/root/.ssh"})
		seenTargets["/root/.ssh"] = true
	}

	return mounts
}

func rewriteGitConfigMount(repoPath string, mounts []sandbox.ConfigMount) error {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path for gitconfig rewrite: %w", err)
	}

	for _, mount := range mounts {
		if mount.Target != "/.gitconfig" {
			continue
		}
		if err := rewriteGitConfigFile(mount.Source, absRepo); err != nil {
			return fmt.Errorf("rewrite mounted gitconfig: %w", err)
		}
		return nil
	}

	return nil
}

func rewriteGitConfigFile(path, absRepo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	updated := strings.ReplaceAll(string(data), absRepo, "/workspace")
	if updated == string(data) {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	return os.WriteFile(path, []byte(updated), info.Mode().Perm())
}

// hydrateGHConfigMount injects an oauth_token into the copied gh hosts.yml
// when the host uses keyring-backed auth (which leaves oauth_token empty in
// the on-disk file). Without this, gh inside the container cannot authenticate.
func hydrateGHConfigMount(ctx context.Context, mounts []sandbox.ConfigMount, lookupGHToken func(context.Context) (string, error)) error {
	for _, mount := range mounts {
		if mount.Target != "/.config/gh" {
			continue
		}
		hostsPath := filepath.Join(mount.Source, "hosts.yml")
		return hydrateGHHostsFile(ctx, hostsPath, lookupGHToken)
	}

	return nil
}

func hydrateGHHostsFile(ctx context.Context, path string, lookupGHToken func(context.Context) (string, error)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read gh hosts file: %w", err)
	}

	var hosts map[string]map[string]any
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		return fmt.Errorf("parse gh hosts file: %w", err)
	}
	if len(hosts) == 0 {
		return nil
	}

	needsToken := true
	for _, cfg := range hosts {
		if token, ok := cfg["oauth_token"].(string); ok && strings.TrimSpace(token) != "" {
			needsToken = false
			break
		}
	}
	if !needsToken {
		return nil
	}

	token, err := lookupGHToken(ctx)
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) && execErr.Err == exec.ErrNotFound {
			return nil
		}
		return fmt.Errorf("resolve gh token for container mount: %w", err)
	}

	targetHost := "github.com"
	if _, ok := hosts[targetHost]; !ok {
		for host := range hosts {
			targetHost = host
			break
		}
	}

	if hosts[targetHost] == nil {
		hosts[targetHost] = map[string]any{}
	}
	hosts[targetHost]["oauth_token"] = token

	updated, err := yaml.Marshal(hosts)
	if err != nil {
		return fmt.Errorf("marshal gh hosts file: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat gh hosts file: %w", err)
	}

	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write gh hosts file: %w", err)
	}

	return nil
}
