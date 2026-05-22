package batch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type gitIdentity struct {
	Name  string
	Email string
}

func resolveGitIdentity(repoPath string) (gitIdentity, error) {
	home, err := os.UserHomeDir()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return gitIdentity{}, fmt.Errorf("resolve home dir for git identity: %w", err)
	}

	identity := gitIdentity{}
	if home != "" {
		identity.Name, err = resolveGitIdentityValue(repoPath, filepath.Join(home, ".gitconfig"), "user.name")
		if err != nil {
			return gitIdentity{}, err
		}
		identity.Email, err = resolveGitIdentityValue(repoPath, filepath.Join(home, ".gitconfig"), "user.email")
		if err != nil {
			return gitIdentity{}, err
		}

		gitConfigDir := hostGitConfigDir(home)
		if strings.TrimSpace(identity.Name) == "" {
			identity.Name, err = resolveGitIdentityValue(repoPath, filepath.Join(gitConfigDir, "config"), "user.name")
			if err != nil {
				return gitIdentity{}, err
			}
		}
		if strings.TrimSpace(identity.Email) == "" {
			identity.Email, err = resolveGitIdentityValue(repoPath, filepath.Join(gitConfigDir, "config"), "user.email")
			if err != nil {
				return gitIdentity{}, err
			}
		}
	}

	if strings.TrimSpace(identity.Name) == "" {
		identity.Name, err = gitConfigValue(repoPath, "--local", "--get", "user.name")
		if err != nil {
			return gitIdentity{}, err
		}
	}
	if strings.TrimSpace(identity.Email) == "" {
		identity.Email, err = gitConfigValue(repoPath, "--local", "--get", "user.email")
		if err != nil {
			return gitIdentity{}, err
		}
	}

	missing := make([]string, 0, 2)
	if strings.TrimSpace(identity.Name) == "" {
		missing = append(missing, "user.name")
	}
	if strings.TrimSpace(identity.Email) == "" {
		missing = append(missing, "user.email")
	}
	if len(missing) > 0 {
		return gitIdentity{}, fmt.Errorf("resolve git identity: missing %s; set them in ~/.gitconfig, %s, or repo-local .git/config", strings.Join(missing, " and "), filepath.Join(hostGitConfigDir("~"), "config"))
	}

	return identity, nil
}

func resolveGitIdentityValue(repoPath, configPath, key string) (string, error) {
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat git config %q: %w", configPath, err)
	}
	return gitConfigValue(repoPath, "--includes", "--file", configPath, "--get", key)
}

func gitConfigValue(repoPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoPath, "config"}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(cmdArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func setGitConfigValue(repoPath string, args ...string) error {
	cmdArgs := append([]string{"-C", repoPath, "config"}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(cmdArgs, " "), err, out)
	}
	return nil
}

func hostGitConfigDir(home string) string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "git")
	}
	return filepath.Join(home, ".config", "git")
}
