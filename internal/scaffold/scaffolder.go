package scaffold

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

const defaultBuildToolsPreset = "generic"

const goBuildToolsPreset = "go"

const DefaultMISEVersion = "v2026.5.8"

const promptMdHeader = `# Context

<!--
  Sandman substitutes these built-in keys before the agent runs:
  {{ISSUE_NUMBER}}   -> the issue number
  {{ISSUE_TITLE}}    -> the issue title
  {{ISSUE_BODY}}     -> the issue body
  {{SOURCE_BRANCH}}  -> the source branch name
  {{TARGET_BRANCH}}  -> the target branch name

  Add custom keys in config.yaml under promptArgs and use them as {{KEY_NAME}}.
  The agent command references the rendered prompt file path as {{.PromptFile}}.

  If a toolchain is missing, use mise first before adding ad hoc installs.
-->

# Task

<!-- Describe what the agent should do. -->

# Done

<!-- When the task is complete, signal termination if your agent supports it. -->
`

// Options configures the scaffolding behavior.
type Options struct {
	BuildTools  string // --build-tools override
	ToolVersion string // --tool-version override
	Agent       string // --agent override
}

// BuildToolsPreset describes a scaffold-time recipe for the container image.
type BuildToolsPreset struct {
	Name           string
	BaseImage      string
	SharedPackages []string
	MiseVersion    string
}

// Prompter asks the user for confirmation or selection interactively.
type Prompter interface {
	Confirm(msg string) (bool, error)
	Select(msg string, options []string) (string, error)
}

// KnownAgents is the sorted list of built-in agent preset keys.
var KnownAgents = func() []string {
	agents := make([]string, 0, len(config.BuiltInAgentPresets))
	for name := range config.BuiltInAgentPresets {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	return agents
}()

var builtInBuildToolsPresets = map[string]BuildToolsPreset{
	defaultBuildToolsPreset: {
		Name:      defaultBuildToolsPreset,
		BaseImage: "debian:bookworm-slim",
		SharedPackages: []string{
			"bash",
			"build-essential",
			"ca-certificates",
			"curl",
			"file",
			"git",
			"nodejs",
			"npm",
			"python3",
			"python3-pip",
			"unzip",
			"xz-utils",
		},
		MiseVersion: DefaultMISEVersion,
	},
	goBuildToolsPreset: {
		Name:      goBuildToolsPreset,
		BaseImage: "debian:bookworm-slim",
		SharedPackages: []string{
			"bash",
			"build-essential",
			"ca-certificates",
			"curl",
			"file",
			"git",
			"nodejs",
			"npm",
			"python3",
			"python3-pip",
			"unzip",
			"xz-utils",
		},
		MiseVersion: DefaultMISEVersion,
	},
}

// DefaultBuiltInAgentVersion returns the latest bundled version pin for a built-in agent.
func DefaultBuiltInAgentVersion(agent string) string {
	versions := builtInAgentVersionCatalog[agent]
	if len(versions) == 0 {
		return ""
	}
	return versions[0]
}

var KnownBuildToolsPresets = func() []string {
	presets := make([]string, 0, len(builtInBuildToolsPresets))
	for name := range builtInBuildToolsPresets {
		presets = append(presets, name)
	}
	sort.Strings(presets)
	return presets
}()

var builtInAgentVersionCatalog = map[string][]string{
	"opencode":    {"1.15.0", "1.14.0", "1.13.0"},
	"claude-code": {"2.1.142", "2.1.120", "2.0.0"},
	"codex":       {"0.130.0", "0.129.0", "0.128.0"},
	"pi":          {"0.1.2", "0.1.1", "0.1.0"},
}

var bundledGoVersionCatalog = map[string]string{
	"latest":      "1.26.3",
	"1.25":        "1.25.13",
	"1.24":        "1.24.13",
	"prefix:1.20": "1.20.14",
}

// Scaffolder creates the .sandman/ directory and its files.
type Scaffolder struct{}

// Scaffold writes config.yaml, Dockerfile, and prompt.md into .sandman/.
func (s *Scaffolder) Scaffold(repoRoot string, opts Options, p Prompter) error {
	sandmanDir := filepath.Join(repoRoot, ".sandman")

	if info, err := os.Stat(sandmanDir); err == nil && info.IsDir() {
		ok, err := p.Confirm("Directory .sandman/ already exists. Overwrite?")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("init cancelled")
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat .sandman: %w", err)
	}

	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		return fmt.Errorf("create .sandman: %w", err)
	}

	agent, err := s.resolveAgent(opts, p)
	if err != nil {
		return err
	}

	preset, err := s.resolveBuildToolsPreset(repoRoot, opts, p)
	if err != nil {
		return err
	}

	goVersion := ""
	agentVersion := DefaultBuiltInAgentVersion(agent)
	if preset.Name == goBuildToolsPreset {
		goVersion, err = s.resolveGoVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else {
		agentVersion, err = s.resolveAgentVersion(agent, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	}

	cfg := &config.Config{
		Agent:             agent,
		BuildTools:        preset.Name,
		DefaultParallel:   config.DefaultParallel,
		ContainerCapacity: config.DefaultContainerCapacity,
		MaxContainers:     config.DefaultMaxContainers,
		WorktreeDir:       config.DefaultWorktreeDir,
		Sandbox:           config.DefaultSandbox,
		AgentProviders: map[string]config.Agent{
			agent: {Preset: agent},
		},
	}

	configPath := filepath.Join(sandmanDir, "config.yaml")
	if err := config.Save(configPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	dockerfile := s.renderBuildToolsDockerfile(preset, agent, agentVersion, goVersion)
	dockerfilePath := filepath.Join(sandmanDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}

	promptPath := filepath.Join(sandmanDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(promptMdHeader), 0644); err != nil {
		return fmt.Errorf("write prompt.md: %w", err)
	}

	return nil
}

func (s *Scaffolder) resolveAgent(opts Options, p Prompter) (string, error) {
	if opts.Agent != "" {
		if _, ok := config.BuiltInAgentPresets[opts.Agent]; !ok {
			return "", fmt.Errorf("unknown agent: %q (supported: %s)", opts.Agent, strings.Join(KnownAgents, ", "))
		}
		return opts.Agent, nil
	}
	return config.DefaultAgent, nil
}

func (s *Scaffolder) resolveBuildToolsPreset(repoRoot string, opts Options, p Prompter) (BuildToolsPreset, error) {
	name := strings.ToLower(strings.TrimSpace(opts.BuildTools))
	if name == "" {
		if hasGoRepoHint(repoRoot) {
			name = goBuildToolsPreset
		} else {
			name = defaultBuildToolsPreset
		}
	}
	preset, ok := builtInBuildToolsPresets[name]
	if !ok {
		return BuildToolsPreset{}, fmt.Errorf("unknown build-tools preset: %q (supported: %s)", opts.BuildTools, strings.Join(KnownBuildToolsPresets, ", "))
	}
	return preset, nil
}

func hasGoRepoHint(repoRoot string) bool {
	_, found, err := readGoVersionHint(repoRoot)
	return err == nil && found
}

func (s *Scaffolder) resolveGoVersion(repoRoot, selector string, p Prompter) (string, error) {
	hint, found, err := readGoVersionHint(repoRoot)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "" {
		if found {
			if p != nil {
				selected, err := p.Select(fmt.Sprintf("Choose a Go version (repo: %s):", hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = normalizeGoVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if p != nil {
				selected, err := p.Select("Choose a Go version:", []string{"latest", "lts"})
				if err == nil {
					choice = normalizeGoVersionSelector(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	resolved, err := resolveGoVersionChoice(choice, hint, found)
	if err != nil {
		return "", fmt.Errorf("resolve go version: %w", err)
	}
	return resolved, nil
}

func resolveGoVersionChoice(choice, hint string, hintFound bool) (string, error) {
	choice = normalizeGoVersionSelector(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo Go version hint found")
		}
		return resolveMiseGoVersion(normalizeGoVersionSelector(hint))
	case "latest", "lts":
		if strings.ToLower(choice) == "latest" {
			return resolveMiseGoVersion("latest")
		}
		latest, err := resolveMiseGoVersion("latest")
		if err != nil {
			return "", err
		}
		prefix, err := goPreviousMinorPrefix(latest)
		if err != nil {
			return "", err
		}
		return resolveMiseGoVersion(prefix)
	}

	return resolveMiseGoVersion(choice)
}

func normalizeGoVersionSelector(selector string) string {
	selector = strings.TrimSpace(selector)
	if len(selector) > 2 && strings.HasPrefix(strings.ToLower(selector), "go") && selector[2] >= '0' && selector[2] <= '9' {
		return selector[2:]
	}
	if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
		return selector[1:]
	}
	return selector
}

func resolveMiseGoVersion(selector string) (string, error) {
	selector = normalizeGoVersionSelector(selector)
	args := []string{"latest"}
	if selector == "" || strings.EqualFold(selector, "latest") {
		args = append(args, "go")
	} else {
		args = append(args, "go@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			return version, nil
		}
	}

	if version, ok := bundledGoVersionCatalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := bundledGoVersionCatalog["latest"]; ok {
			return version, nil
		}
	}
	return "", fmt.Errorf("resolve go version %q: %w", selector, err)
}

func goPreviousMinorPrefix(version string) (string, error) {
	version = normalizeGoVersionSelector(version)
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected Go version %q", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("parse Go major version %q: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("parse Go minor version %q: %w", version, err)
	}
	if minor == 0 {
		return "", fmt.Errorf("unexpected Go version %q", version)
	}
	minor--

	prefix := fmt.Sprintf("%d.%d", major, minor)
	if major == 1 && minor <= 20 {
		return "prefix:" + prefix, nil
	}
	return prefix, nil
}

func readGoVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".go-version", "go.mod", "go.work", ".tool-versions"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Go version hint from %s: %w", rel, err)
		}
		if version, ok := parseGoVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseGoVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".go-version":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			return line, true
		}
	case ".tool-versions":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "go" {
				return fields[1], true
			}
		}
	default:
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "go ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					return fields[1], true
				}
			}
		}
	}
	return "", false
}

func (s *Scaffolder) resolveAgentVersion(agent, selector string, p Prompter) (string, error) {
	versions, ok := builtInAgentVersionCatalog[agent]
	if !ok || len(versions) == 0 {
		return "", fmt.Errorf("unknown built-in agent version catalog for %q", agent)
	}

	choice := strings.TrimSpace(selector)
	if choice == "" && p != nil {
		selected, err := p.Select("Choose a built-in agent version:", append([]string{"latest", "lts"}, versions...))
		if err == nil {
			choice = strings.TrimSpace(selected)
		}
	}
	if choice == "" {
		choice = "latest"
	}

	resolved, err := resolveVersionChoice(choice, versions)
	if err != nil {
		return "", fmt.Errorf("resolve tool version: %w", err)
	}
	return resolved, nil
}

func resolveVersionChoice(choice string, versions []string) (string, error) {
	choice = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(choice), "v"))
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch choice {
	case "latest":
		return versions[0], nil
	case "lts":
		if len(versions) > 1 {
			return versions[1], nil
		}
		return versions[0], nil
	}

	parts := strings.Split(choice, ".")
	if len(parts) > 3 {
		return "", fmt.Errorf("unsupported version selector %q", choice)
	}
	if len(parts) == 3 {
		return choice, nil
	}

	prefix := choice + "."
	for _, version := range versions {
		if strings.HasPrefix(version, prefix) {
			return version, nil
		}
	}

	return "", fmt.Errorf("no version matching %q", choice)
}

func (s *Scaffolder) renderBuildToolsDockerfile(preset BuildToolsPreset, agent, agentVersion, goVersion string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# sandman build-tools: %s\n", preset.Name)
	fmt.Fprintf(&out, "# sandman agent-provider: %s\n", agent)
	if preset.Name == goBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman go-version: %s\n", goVersion)
	}
	fmt.Fprintf(&out, "# sandman tool-version: %s\n", agentVersion)
	fmt.Fprintf(&out, "# sandman mise-version: %s\n", preset.MiseVersion)
	fmt.Fprintf(&out, "FROM %s\n", preset.BaseImage)
	fmt.Fprintf(&out, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(preset.SharedPackages, " "))
	fmt.Fprintf(&out, "RUN MISE_VERSION=%s curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh\n", preset.MiseVersion)
	fmt.Fprintf(&out, "ENV PATH=\"/root/.local/share/mise/bin:/root/.local/share/mise/shims:/root/.local/bin:$PATH\"\n")
	out.WriteString("WORKDIR /app\n")
	if preset.Name == goBuildToolsPreset {
		out.WriteString(renderGoInstallCommand(goVersion))
	}
	out.WriteString(renderAgentInstallCommand(agent, agentVersion))
	return out.String()
}

func renderGoInstallCommand(version string) string {
	return fmt.Sprintf("RUN mise use -g --pin go@%s\n", version)
}

func renderAgentInstallCommand(agent, version string) string {
	switch agent {
	case "opencode":
		return fmt.Sprintf("RUN npm install -g opencode-ai@%s\n", version)
	case "claude-code":
		return fmt.Sprintf("RUN npm install -g @anthropic-ai/claude-code@%s\n", version)
	case "codex":
		return fmt.Sprintf("RUN npm install -g @openai/codex@%s\n", version)
	case "pi":
		return fmt.Sprintf("RUN python3 -m pip install --break-system-packages pi==%s\n", version)
	default:
		return ""
	}
}

// ValidateDockerfileMetadata fails when scaffold metadata drift is detected.
// Metadata-free Dockerfiles are treated as opaque custom files.
// tool-version and mise-version are intentionally not validated here because
// runtime config has no canonical pinned value to compare against.
func ValidateDockerfileMetadata(repoRoot, expectedBuildTools, expectedAgent string) error {
	if strings.TrimSpace(expectedBuildTools) == "" {
		expectedBuildTools = defaultBuildToolsPreset
	}
	dockerfilePath := filepath.Join(repoRoot, ".sandman", "Dockerfile")
	meta, found, err := readDockerfileMetadata(dockerfilePath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if meta.BuildToolsPreset != expectedBuildTools {
		return fmt.Errorf("scaffold metadata drift: Dockerfile build-tools %q does not match expected %q", meta.BuildToolsPreset, expectedBuildTools)
	}
	if meta.AgentProvider != expectedAgent {
		return fmt.Errorf("scaffold metadata drift: Dockerfile agent-provider %q does not match config agent %q", meta.AgentProvider, expectedAgent)
	}
	return nil
}

type dockerfileMetadata struct {
	BuildToolsPreset string
	AgentProvider    string
	GoVersion        string
	ToolVersion      string
	MiseVersion      string
}

func readDockerfileMetadata(path string) (dockerfileMetadata, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return dockerfileMetadata{}, false, nil
		}
		return dockerfileMetadata{}, false, fmt.Errorf("read Dockerfile metadata: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	meta := dockerfileMetadata{}
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if found {
				break
			}
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if found {
				break
			}
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if !strings.HasPrefix(strings.ToLower(text), "sandman ") {
			if found {
				break
			}
			continue
		}
		found = true
		kv := strings.TrimSpace(text[len("sandman "):])
		key, value, ok := strings.Cut(kv, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(strings.ToLower(key)) {
		case "build-tools":
			meta.BuildToolsPreset = strings.TrimSpace(value)
		case "agent-provider":
			meta.AgentProvider = strings.TrimSpace(value)
		case "tool-version":
			meta.ToolVersion = strings.TrimSpace(value)
		case "go-version":
			meta.GoVersion = strings.TrimSpace(value)
		case "mise-version":
			meta.MiseVersion = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return dockerfileMetadata{}, false, fmt.Errorf("scan Dockerfile metadata: %w", err)
	}
	return meta, found, nil
}
