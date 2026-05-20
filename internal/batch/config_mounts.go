package batch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

func prepareContainerConfigMounts(repoPath string, opts *sandbox.StartOptions) (func(), error) {
	dirs := append([]string(nil), opts.AgentConfigDirs...)
	files := append([]string(nil), opts.AgentConfigFiles...)

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

	opts.ConfigMounts = mounts
	return cleanup, nil
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
