package batch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"gopkg.in/yaml.v3"
)

var lookupGHToken = func() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh auth token returned empty token")
	}
	return token, nil
}

// PrepareContainerConfigMounts resolves git, gh, ssh, and agent config paths
// into temp-copied ConfigMounts so container runs do not bind host paths
// directly and can safely follow symlinked config trees.
func PrepareContainerConfigMounts(repoPath string, opts *sandbox.StartOptions, gitCfg config.GitConfig) (func(), error) {
	dirs := append([]string(nil), opts.AgentConfigDirs...)
	files := append([]string(nil), opts.AgentConfigFiles...)
	var extraCleanup []func()

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

	mounts, cleanup, err := sandbox.ResolveConfigMounts(dirs, files)
	if err != nil {
		return nil, fmt.Errorf("resolve config mounts: %w", err)
	}
	mounts = addSSHMountAliases(mounts)

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

	if err := hydrateGHConfigMount(mounts); err != nil {
		cleanup()
		return nil, err
	}
	updatedMounts, cleanupFn, err := prepareContainerGitIdentityMounts(mounts, gitCfg)
	if err != nil {
		cleanup()
		return nil, err
	}
	if cleanupFn != nil {
		extraCleanup = append(extraCleanup, cleanupFn)
	}
	mounts = updatedMounts

	opts.ConfigMounts = mounts
	return func() {
		for _, fn := range extraCleanup {
			fn()
		}
		cleanup()
	}, nil
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
func hydrateGHConfigMount(mounts []sandbox.ConfigMount) error {
	for _, mount := range mounts {
		if mount.Target != "/.config/gh" {
			continue
		}
		hostsPath := filepath.Join(mount.Source, "hosts.yml")
		return hydrateGHHostsFile(hostsPath)
	}

	return nil
}

func hydrateGHHostsFile(path string) error {
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

	token, err := lookupGHToken()
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

func prepareContainerGitIdentityMounts(mounts []sandbox.ConfigMount, gitCfg config.GitConfig) ([]sandbox.ConfigMount, func(), error) {
	if strings.TrimSpace(gitCfg.AuthorName) == "" || strings.TrimSpace(gitCfg.AuthorEmail) == "" {
		return mounts, nil, nil
	}

	var cleanup func()
	configPaths := containerGitConfigPaths(mounts)
	if len(configPaths) == 0 {
		tmpDir, err := os.MkdirTemp("", "sandman-git-config-*")
		if err != nil {
			return nil, nil, fmt.Errorf("create temp git config dir: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(tmpDir) }

		gitDir := filepath.Join(tmpDir, ".config", "git")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("create temp git config path: %w", err)
		}
		mounts = append(mounts, sandbox.ConfigMount{Source: gitDir, Target: "/.config/git"})
		configPaths = append(configPaths, filepath.Join(gitDir, "config"))
	}

	for _, path := range configPaths {
		if err := writeGitIdentityConfig(path, gitCfg); err != nil {
			if cleanup != nil {
				cleanup()
			}
			return nil, nil, err
		}
	}

	return mounts, cleanup, nil
}

func containerGitConfigPaths(mounts []sandbox.ConfigMount) []string {
	seen := map[string]bool{}
	var paths []string
	for _, mount := range mounts {
		var path string
		switch mount.Target {
		case "/.gitconfig":
			path = mount.Source
		case "/.config/git":
			path = filepath.Join(mount.Source, "config")
		case "/.config/git/config":
			path = mount.Source
		}
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

func writeGitIdentityConfig(path string, gitCfg config.GitConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create git config dir %q: %w", filepath.Dir(path), err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			return fmt.Errorf("create git config file %q: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("stat git config file %q: %w", path, err)
	}

	for _, kv := range []struct{ key, value string }{
		{"user.name", gitCfg.AuthorName},
		{"user.email", gitCfg.AuthorEmail},
	} {
		cmd := exec.Command("git", "config", "--file", path, kv.key, kv.value)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config --file %s %s: %w\n%s", path, kv.key, err, out)
		}
	}

	return nil
}
