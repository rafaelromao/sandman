package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

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
-->

# Task

<!-- Describe what the agent should do. -->

# Done

<!-- When the task is complete, signal termination if your agent supports it. -->
`

// Options configures the scaffolding behavior.
type Options struct {
	Lang      string // --lang override
	FromImage string // --from-image override
	Agent     string // --agent override
}

// Prompter asks the user for confirmation or selection interactively.
type Prompter interface {
	Confirm(msg string) (bool, error)
	Select(msg string, options []string) (string, error)
}

type languageDetector struct {
	detect func(string) bool
	lang   string
}

func fileExists(name string) func(string) bool {
	return func(root string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
}

func globExists(pattern string) func(string) bool {
	return func(root string) bool {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		return err == nil && len(matches) > 0
	}
}

var languageDetectors = []languageDetector{
	{fileExists("go.mod"), "go"},
	{fileExists("package.json"), "node"},
	{fileExists("requirements.txt"), "python"},
	{fileExists("Cargo.toml"), "rust"},
	{fileExists("pom.xml"), "java"},
	{fileExists("build.gradle"), "java"},
	{globExists("*.csproj"), "dotnet"},
	{globExists("*.fsproj"), "dotnet"},
	{globExists("*.sln"), "dotnet"},
	{fileExists("composer.json"), "php"},
	{fileExists("mix.exs"), "elixir"},
	{fileExists("build.zig"), "zig"},
	{fileExists("Gemfile"), "ruby"},
	{fileExists("Package.swift"), "swift"},
	{fileExists("CMakeLists.txt"), "cpp"},
	{fileExists("Makefile"), "cpp"},
	{fileExists("project.clj"), "clojure"},
	{fileExists("deps.edn"), "clojure"},
	{fileExists("build.gradle.kts"), "kotlin"},
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

var agentInstallCommands = map[string]string{
	"opencode":    `RUN curl -fsSL https://opencode.ai/install.sh | sh`,
	"claude-code": `RUN npm install -g @anthropic-ai/claude-code`,
	"codex":       `RUN npm install -g codex`,
	"pi":          `RUN pip install pi`,
}

var baseImages = map[string]string{
	"go":      "golang:latest",
	"node":    "node:latest",
	"python":  "python:latest",
	"rust":    "rust:latest",
	"java":    "maven:latest",
	"dotnet":  "mcr.microsoft.com/dotnet/sdk:latest",
	"php":     "php:latest",
	"elixir":  "elixir:latest",
	"zig":     "ziglang/zig:latest",
	"ruby":    "ruby:latest",
	"swift":   "swift:latest",
	"cpp":     "gcc:latest",
	"clojure": "clojure:latest",
	"kotlin":  "gradle:latest",
}

// KnownLanguages is the alphabetically sorted list of supported languages for prompts and validation.
var KnownLanguages = func() []string {
	langs := make([]string, 0, len(baseImages))
	for lang := range baseImages {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}()

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

	cfg := &config.Config{
		Agent:             agent,
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

	lang, err := s.resolveLanguage(repoRoot, opts, p)
	if err != nil {
		return err
	}

	dockerfile := s.renderDockerfile(lang, opts.FromImage, agent)
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

func (s *Scaffolder) resolveLanguage(repoRoot string, opts Options, p Prompter) (string, error) {
	if opts.Lang != "" {
		if _, ok := baseImages[opts.Lang]; !ok {
			return "", fmt.Errorf("unknown language: %q (supported: %s)", opts.Lang, strings.Join(KnownLanguages, ", "))
		}
		return opts.Lang, nil
	}

	seen := make(map[string]bool)
	var detected []string
	for _, d := range languageDetectors {
		if d.detect(repoRoot) && !seen[d.lang] {
			seen[d.lang] = true
			detected = append(detected, d.lang)
		}
	}

	// Deprioritize C/C++ from Makefile when other languages are present,
	// unless CMakeLists.txt also exists (stronger signal for C/C++).
	if len(detected) > 1 {
		hasCMakeLists := fileExists("CMakeLists.txt")(repoRoot)
		if !hasCMakeLists {
			filtered := make([]string, 0, len(detected))
			for _, lang := range detected {
				if lang != "cpp" {
					filtered = append(filtered, lang)
				}
			}
			detected = filtered
		}
	}

	if len(detected) == 1 {
		return detected[0], nil
	}

	if len(detected) > 1 {
		return p.Select("Multiple languages detected. Choose one:", detected)
	}

	return p.Select("No language detected. Choose one:", KnownLanguages)
}

func (s *Scaffolder) renderDockerfile(lang, fromImage, agent string) string {
	var out string
	if fromImage != "" {
		out = fmt.Sprintf("FROM %s\n", fromImage)
	} else if img, ok := baseImages[lang]; ok {
		out = fmt.Sprintf("FROM %s\n", img)
	} else {
		out = "FROM ubuntu:latest\n"
	}
	out += "WORKDIR /app\n"
	if cmd, ok := agentInstallCommands[agent]; ok {
		out += cmd + "\n"
	}
	return out
}
