package scaffold

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

const defaultBuildToolsPreset = "generic"

const goBuildToolsPreset = "go"

const dotnetBuildToolsPreset = "dotnet"

const nodeBuildToolsPreset = "node"

const pythonBuildToolsPreset = "python"

const elixirBuildToolsPreset = "elixir"

const rubyBuildToolsPreset = "ruby"

const rustBuildToolsPreset = "rust"

const javaBuildToolsPreset = "java"

const DefaultMISEVersion = "v2026.5.8"

const DefaultRTKVersion = "v0.42.0"

// Options configures the scaffolding behavior.
type Options struct {
	BuildTools      string // --build-tools override
	ToolVersion     string // --tool-version override
	Agent           string // --agent override
	Model           string // --model override
	Parallel        int    // --parallel override (-1 = use config default)
	ParallelReviews int    // --parallel-reviews override (-1 = use config default)
	ReviewCommand   string // --review-command override
	Retries         *int   // --retries override; nil = use config.DefaultRetries
	RunIdleTimeout  *int   // --run-idle-timeout override; nil = use config.DefaultRunIdleTimeout
}

// BuildToolsPreset describes a scaffold-time recipe for the container image.
type BuildToolsPreset struct {
	Name           string
	BaseImage      string
	SharedPackages []string
	ExtraPackages  []string
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

// sharedPackages is the baseline apt package set installed for every
// BuildToolsPreset. All entries of builtInBuildToolsPresets reference this
// slice; do not mutate it in place or the change will leak across presets.
var sharedPackages = []string{
	"bash",
	"build-essential",
	"ca-certificates",
	"curl",
	"file",
	"gh",
	"git",
	"jq",
	"nodejs",
	"npm",
	"openssh-client",
	"python3",
	"python3-pip",
	"ripgrep",
	"unzip",
	"xz-utils",
	"yq",
}

var builtInBuildToolsPresets = map[string]BuildToolsPreset{
	defaultBuildToolsPreset: {
		Name:           defaultBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	goBuildToolsPreset: {
		Name:           goBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	dotnetBuildToolsPreset: {
		Name:           dotnetBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	nodeBuildToolsPreset: {
		Name:           nodeBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	pythonBuildToolsPreset: {
		Name:           pythonBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	elixirBuildToolsPreset: {
		Name:           elixirBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		ExtraPackages:  []string{"libncurses-dev"},
		MiseVersion:    DefaultMISEVersion,
	},
	rubyBuildToolsPreset: {
		Name:           rubyBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	rustBuildToolsPreset: {
		Name:           rustBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
	},
	javaBuildToolsPreset: {
		Name:           javaBuildToolsPreset,
		BaseImage:      "debian:bookworm-slim",
		SharedPackages: sharedPackages,
		MiseVersion:    DefaultMISEVersion,
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
	"opencode": {"1.15.0", "1.14.0", "1.13.0"},
}

var bundledGoVersionCatalog = map[string]string{
	"latest":      "1.26.3",
	"1.25":        "1.25.13",
	"1.24":        "1.24.13",
	"prefix:1.20": "1.20.14",
}

func BundledGoVersionLatest() string {
	return bundledGoVersionCatalog["latest"]
}

var bundledNodeVersionCatalog = map[string]string{
	"latest": "24.2.0",
	"lts":    "22.16.0",
	"24":     "24.2.0",
	"22":     "22.16.0",
	"20":     "20.19.0",
	"18":     "18.20.8",
}

var bundledPythonVersionCatalog = map[string]string{
	"latest": "3.13.3",
	"3.14":   "3.14.3",
	"3.13":   "3.13.3",
	"3.12":   "3.12.9",
	"3.11":   "3.11.11",
	"3.10":   "3.10.16",
	"3.9":    "3.9.21",
}

var bundledDotnetVersionCatalog = map[string]string{
	"latest": "10.0.100",
	"lts":    "8.0.416",
	"10":     "10.0.100",
	"10.0":   "10.0.100",
	"9":      "9.0.203",
	"9.0":    "9.0.203",
	"8":      "8.0.416",
	"8.0":    "8.0.416",
	"7":      "7.0.410",
	"7.0":    "7.0.410",
	"6":      "6.0.428",
	"6.0":    "6.0.428",
}

var bundledElixirVersionCatalog = map[string]string{
	"latest": "1.20.2-otp-29",
	"lts":    "1.18.4-otp-28",
	"1.20":   "1.20.2-otp-29",
	"1.19":   "1.19.5-otp-28",
	"1.18":   "1.18.4-otp-28",
	"1.17":   "1.17.3-otp-27",
	"1.16":   "1.16.3-otp-26",
	"1.15":   "1.15.8-otp-26",
}

var bundledRubyVersionCatalog = map[string]string{
	"latest": "3.4.5",
	"lts":    "3.3.8",
	"3.4":    "3.4.5",
	"3.3":    "3.3.8",
	"3.2":    "3.2.8",
	"3.1":    "3.1.7",
}

var bundledRustVersionCatalog = map[string]string{
	"latest":  "1.96.1",
	"lts":     "1.95.0",
	"stable":  "1.96.1",
	"beta":    "1.96.1",
	"nightly": "1.96.1",
	"1.96":    "1.96.1",
	"1.96.1":  "1.96.1",
	"1.96.0":  "1.96.0",
	"1.95":    "1.95.0",
	"1.95.0":  "1.95.0",
	"1.94":    "1.94.0",
	"1.94.0":  "1.94.0",
}

var bundledJavaVersionCatalog = map[string]string{
	"latest":  "21.0.2",
	"lts":     "17.0.10",
	"21":      "21.0.2",
	"21.0":    "21.0.2",
	"21.0.2":  "21.0.2",
	"17":      "17.0.10",
	"17.0":    "17.0.10",
	"17.0.10": "17.0.10",
	"11":      "11.0.22",
	"11.0":    "11.0.22",
	"11.0.22": "11.0.22",
	"8":       "8.0.412",
	"8.0":     "8.0.412",
	"8.0.412": "8.0.412",
}

// bundledElixirOTPMap pairs each cataloged Elixir major.minor with the
// Erlang/OTP release that ships with it. deriveErlangOTPFromElixir uses
// it as the offline fallback when the resolved Elixir version lacks the
// `-otp-<NN>` suffix (e.g. user-supplied `~> 1.18` or older non-tagged
// versions).
var bundledElixirOTPMap = map[string]string{
	"1.20": "29",
	"1.19": "28",
	"1.18": "28",
	"1.17": "27",
	"1.16": "26",
	"1.15": "26",
}

const bundledElixirDefaultOTP = "29"

var nodeVersionSelectorPattern = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)

var rustVersionSelectorPattern = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)

var javaVersionSelectorPattern = regexp.MustCompile(`^\d+(\.\d+){0,2}(\+\d+)?$`)

var elixirVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

// Scaffolder creates the .sandman/ directory and its files.
type Scaffolder struct{}

// Scaffold writes config.yaml, Dockerfile, and prompt.md into .sandman/.
//
// The soft migration that copied a pre-existing
// `.sandman/priority-selection-prompt.md` to `.sandman/auto-selection-prompt.md`
// is no longer performed; operators with a customized legacy file must rename
// it to `.sandman/auto-selection-prompt.md` manually before re-running `init`.
func (s *Scaffolder) Scaffold(repoRoot string, opts Options, p Prompter) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	sandmanDir := layout.SandmanDir

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

	defaultAgent, err := s.resolveDefaultAgent(opts)
	if err != nil {
		return err
	}

	preset, err := s.resolveBuildToolsPreset(repoRoot, opts, p)
	if err != nil {
		return err
	}

	goVersion := ""
	dotnetVersion := ""
	nodeVersion := ""
	pythonVersion := ""
	elixirVersion := ""
	erlangVersion := ""
	rubyVersion := ""
	rustVersion := ""
	javaVersion := ""
	if preset.Name == goBuildToolsPreset {
		goVersion, err = s.resolveGoVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == dotnetBuildToolsPreset {
		dotnetVersion, err = s.resolveDotnetVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == nodeBuildToolsPreset {
		nodeVersion, err = s.resolveNodeVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == pythonBuildToolsPreset {
		pythonVersion, err = s.resolvePythonVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == elixirBuildToolsPreset {
		elixirVersion, err = s.resolveElixirVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
		erlangVersion, err = deriveErlangOTPFromElixir(elixirVersion)
		if err != nil {
			return err
		}
	} else if preset.Name == rubyBuildToolsPreset {
		rubyVersion, err = s.resolveRubyVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == rustBuildToolsPreset {
		rustVersion, err = s.resolveRustVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	} else if preset.Name == javaBuildToolsPreset {
		javaVersion, err = s.resolveJavaVersion(repoRoot, opts.ToolVersion, p)
		if err != nil {
			return err
		}
	}

	parallel := config.DefaultParallel
	if opts.Parallel > 0 {
		parallel = opts.Parallel
	}
	reviewParallel := config.DefaultReviewParallel
	if opts.ParallelReviews > 0 {
		reviewParallel = opts.ParallelReviews
	}
	model := opts.Model
	if model == "" {
		model = config.DefaultModel
	}

	retries, err := resolveRetries(opts.Retries)
	if err != nil {
		return err
	}
	runIdleTimeout, err := resolveRunIdleTimeout(opts.RunIdleTimeout)
	if err != nil {
		return err
	}
	cfg := &config.Config{
		DefaultAgent:          defaultAgent,
		DefaultModel:          model,
		DefaultReviewAgent:    config.DefaultReviewAgent,
		DefaultReviewModel:    config.DefaultReviewModel,
		BuildTools:            preset.Name,
		ReviewCommand:         effectiveReviewCommand(opts.ReviewCommand),
		DefaultParallel:       parallel,
		DefaultReviewParallel: reviewParallel,
		StartDelay:            config.DefaultStartDelay,
		RunIdleTimeout:        runIdleTimeout,
		Retries:               retries,
		ContainerCapacity:     config.DefaultContainerCapacity,
		MaxContainers:         config.DefaultMaxContainers,
		WorktreeDir:           config.DefaultWorktreeDir,
		Sandbox:               config.DefaultSandbox,
		Git: config.GitConfig{
			BaseBranch: "main",
		},
	}

	configPath := layout.ConfigPath()
	if err := config.Save(configPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	dockerfile := s.renderBuildToolsDockerfile(preset, defaultAgent, goVersion, dotnetVersion, nodeVersion, pythonVersion, elixirVersion, erlangVersion, rubyVersion, rustVersion, javaVersion)
	dockerfilePath := layout.DockerfilePath()
	if err := atomicfs.WriteAtomic(dockerfilePath, []byte(dockerfile), 0644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}

	promptPath := layout.PromptPath()
	if err := atomicfs.WriteAtomic(promptPath, []byte(prompt.DefaultPrompt()), 0644); err != nil {
		return fmt.Errorf("write prompt.md: %w", err)
	}

	autoPromptPath := layout.AutoSelectionPromptPath()
	if _, err := os.Stat(autoPromptPath); os.IsNotExist(err) {
		if err := atomicfs.WriteAtomic(autoPromptPath, []byte(prompt.DefaultPriorityPrompt()), 0644); err != nil {
			return fmt.Errorf("write auto-selection-prompt.md: %w", err)
		}
	}

	if err := s.materializeReviewPrompts(layout); err != nil {
		return err
	}

	return nil
}

// materializeReviewPrompts writes the static PR review prompt and the
// quality rules to .sandman/reviews/. Both files are materialised at init
// time (not on first daemon use) so the user can edit them before any
// review runs. Existing files are left untouched to respect user edits
// across re-inits.
func (s *Scaffolder) materializeReviewPrompts(layout paths.Layout) error {
	reviewsDir := layout.ReviewsDir()
	if err := os.MkdirAll(reviewsDir, 0755); err != nil {
		return fmt.Errorf("create reviews dir: %w", err)
	}
	if err := writeIfMissing(layout.ReviewPromptPath(), []byte(prompt.DefaultPRReviewPrompt())); err != nil {
		return err
	}
	if err := writeIfMissing(layout.QualityRulesPath(), []byte(prompt.DefaultQualityRules())); err != nil {
		return err
	}
	return nil
}

// writeIfMissing writes data to path via a temp file + rename only if no
// file already exists at path. If the file exists, the existing content
// is preserved so user edits survive re-inits.
func writeIfMissing(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return atomicfs.WriteAtomic(path, data, 0644)
}

func effectiveReviewCommand(value string) string {
	if strings.TrimSpace(value) == "" {
		return config.DefaultReviewCommand
	}
	return value
}

func resolveRetries(override *int) (int, error) {
	if override == nil {
		return config.DefaultRetries, nil
	}
	if *override < 0 {
		return 0, fmt.Errorf("retries must be 0 or greater")
	}
	return *override, nil
}

func resolveRunIdleTimeout(override *int) (int, error) {
	if override == nil {
		return config.DefaultRunIdleTimeout, nil
	}
	if *override < 0 {
		return 0, fmt.Errorf("run_idle_timeout must be 0 or greater")
	}
	return *override, nil
}

func (s *Scaffolder) resolveDefaultAgent(opts Options) (string, error) {
	if opts.Agent == "" {
		return config.DefaultAgent, nil
	}
	if _, ok := config.BuiltInAgentPresets[opts.Agent]; !ok {
		return "", fmt.Errorf("unknown default agent: %q (supported: %s)", opts.Agent, strings.Join(KnownAgents, ", "))
	}
	return opts.Agent, nil
}

func (s *Scaffolder) resolveBuildToolsPreset(repoRoot string, opts Options, p Prompter) (BuildToolsPreset, error) {
	name := strings.ToLower(strings.TrimSpace(opts.BuildTools))
	if name == "" {
		if hasDotnetRepoHint(repoRoot) {
			name = dotnetBuildToolsPreset
		} else if hasGoRepoHint(repoRoot) {
			name = goBuildToolsPreset
		} else if hasNodeRepoHint(repoRoot) {
			if p != nil {
				options := []string{nodeBuildToolsPreset}
				for _, preset := range KnownBuildToolsPresets {
					if preset != nodeBuildToolsPreset {
						options = append(options, preset)
					}
				}
				selected, err := p.Select("Choose a build tools preset:", options)
				if err == nil {
					name = strings.ToLower(strings.TrimSpace(selected))
				}
			}
			if name == "" {
				name = nodeBuildToolsPreset
			}
		} else if hasPythonRepoHint(repoRoot) {
			name = pythonBuildToolsPreset
		} else if hasElixirRepoHint(repoRoot) {
			name = elixirBuildToolsPreset
		} else if hasRubyRepoHint(repoRoot) {
			name = rubyBuildToolsPreset
		} else if hasRustRepoHint(repoRoot) {
			if p != nil {
				options := []string{rustBuildToolsPreset}
				for _, preset := range KnownBuildToolsPresets {
					if preset != rustBuildToolsPreset {
						options = append(options, preset)
					}
				}
				selected, err := p.Select("Choose a build tools preset:", options)
				if err == nil {
					name = strings.ToLower(strings.TrimSpace(selected))
				}
			}
			if name == "" {
				name = rustBuildToolsPreset
			}
		} else if hasJavaRepoHint(repoRoot) {
			name = javaBuildToolsPreset
		} else if p != nil {
			selected, err := p.Select("Choose a build tools preset:", KnownBuildToolsPresets)
			if err == nil {
				name = strings.ToLower(strings.TrimSpace(selected))
			}
			if name == "" {
				name = defaultBuildToolsPreset
			}
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

func hasDotnetRepoHint(repoRoot string) bool {
	if _, found, err := readDotnetVersionHint(repoRoot); err == nil && found {
		return true
	}
	match := false
	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		name := d.Name()
		switch {
		case name == "global.json", name == "Directory.Build.props", name == "Directory.Build.targets", name == "Directory.Packages.props":
			match = true
			return fs.SkipAll
		case strings.HasSuffix(strings.ToLower(name), ".csproj"), strings.HasSuffix(strings.ToLower(name), ".fsproj"), strings.HasSuffix(strings.ToLower(name), ".vbproj"), strings.HasSuffix(strings.ToLower(name), ".sln"), strings.HasSuffix(strings.ToLower(name), ".slnx"):
			match = true
			return fs.SkipAll
		default:
			return nil
		}
	})
	return match
}

func hasNodeRepoHint(repoRoot string) bool {
	if _, err := os.Stat(filepath.Join(repoRoot, "package.json")); err == nil {
		return true
	}
	for _, rel := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", ".node-version", ".nvmrc"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	if _, found, err := readNodeVersionHint(repoRoot); err == nil && found {
		return true
	}
	return false
}

func hasPythonRepoHint(repoRoot string) bool {
	for _, rel := range []string{"pyproject.toml", "setup.py", "setup.cfg", "Pipfile", ".python-version"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	return false
}

func hasRubyRepoHint(repoRoot string) bool {
	for _, rel := range []string{"Gemfile", ".ruby-version"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	if _, found, err := readRubyVersionHint(repoRoot); err == nil && found {
		return true
	}
	match := false
	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(strings.ToLower(name), ".gemspec") {
			match = true
			return fs.SkipAll
		}
		return nil
	})
	return match
}

func hasRustRepoHint(repoRoot string) bool {
	for _, rel := range []string{"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml", ".rust-version", ".tool-versions"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	if _, found, err := readRustVersionHint(repoRoot); err == nil && found {
		return true
	}
	return false
}

func hasJavaRepoHint(repoRoot string) bool {
	for _, rel := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	return false
}

func readJavaVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".tool-versions", "pom.xml", "build.gradle", "build.gradle.kts"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Java version hint from %s: %w", rel, err)
		}
		if version, ok := parseJavaVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseJavaVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".tool-versions":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "java" {
				return fields[1], true
			}
		}
	case "pom.xml":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			for _, key := range []string{"<java.version>", "<maven.compiler.source>", "<maven.compiler.target>"} {
				if strings.Contains(line, key) {
					if v := extractXMLTagValue(line, key); v != "" {
						return v, true
					}
				}
			}
		}
	case "build.gradle", "build.gradle.kts":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			for _, key := range []string{"sourceCompatibility", "targetCompatibility", "jvmTarget"} {
				if idx := strings.Index(line, key); idx >= 0 {
					rest := line[idx+len(key):]
					if v := extractJavaDSLValue(rest); v != "" {
						return v, true
					}
				}
			}
		}
	}
	return "", false
}

func extractXMLTagValue(line, key string) string {
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key):]
	rest = strings.TrimSpace(rest)
	endIdx := strings.Index(rest, "</")
	if endIdx < 0 {
		endIdx = strings.Index(rest, "/>")
		if endIdx < 0 {
			return ""
		}
	}
	value := strings.TrimSpace(rest[:endIdx])
	value = strings.Trim(value, "'\"")
	return value
}

func extractJavaDSLValue(rest string) string {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "=") {
		return ""
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	if rest == "" {
		return ""
	}
	first := rest[0]
	if first == '"' || first == '\'' {
		close := strings.IndexRune(rest[1:], rune(first))
		if close < 0 {
			return ""
		}
		return rest[1 : 1+close]
	}
	// Match `JavaVersion.VERSION_<n>` (e.g. `JavaVersion.VERSION_21`) and
	// return the trailing integer. The quoted-string branch above handles
	// `JavaVersion.toVersion("21")`-style expressions.
	if m := regexp.MustCompile(`JavaVersion\.VERSION_(\d+)`).FindStringSubmatch(rest); len(m) == 2 {
		return m[1]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	if strings.Contains(fields[0], ".") {
		parts := strings.Split(fields[0], ".")
		last := parts[len(parts)-1]
		if _, err := strconv.Atoi(last); err == nil {
			return last
		}
	}
	return ""
}

func readRubyVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".ruby-version", ".tool-versions", "Gemfile"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Ruby version hint from %s: %w", rel, err)
		}
		if version, ok := parseRubyVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseRubyVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".ruby-version":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "ruby-")
			return line, true
		}
	case ".tool-versions":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "ruby" {
				return fields[1], true
			}
		}
	case "Gemfile":
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "ruby ") {
				re := regexp.MustCompile(`ruby\s+["']([0-9]+(?:\.[0-9]+){0,2})["']`)
				if m := re.FindStringSubmatch(line); len(m) == 2 {
					return m[1], true
				}
			}
		}
	}
	return "", false
}

func readRustVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{"rust-toolchain.toml", "rust-toolchain", ".rust-version", ".tool-versions", "Cargo.toml"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Rust version hint from %s: %w", rel, err)
		}
		if version, ok := parseRustVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseRustVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".rust-version":
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
			if len(fields) >= 2 && fields[0] == "rust" {
				return fields[1], true
			}
		}
	case "rust-toolchain", "rust-toolchain.toml":
		if version := parseRustToolchainVersion(data); version != "" {
			return version, true
		}
	case "Cargo.toml":
		if version := parseRustCargoVersion(data); version != "" {
			return version, true
		}
	}
	return "", false
}

func parseRustToolchainVersion(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			continue
		}
		if strings.Contains(line, "channel") {
			if _, value, ok := strings.Cut(line, "="); ok {
				value = strings.TrimSpace(value)
				value = strings.Trim(value, "\"'")
				if value != "" {
					return value
				}
			}
			continue
		}
		return strings.TrimPrefix(line, "rust-")
	}
	return ""
}

func parseRustCargoVersion(data []byte) string {
	if match := regexp.MustCompile(`(?m)^\s*rust-version\s*=\s*["']([^"']+)["']\s*$`).FindStringSubmatch(string(data)); len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func hasElixirRepoHint(repoRoot string) bool {
	for _, rel := range []string{"mix.exs", ".formatter.exs", ".elixir_version"} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	if _, found, err := readElixirVersionHint(repoRoot); err == nil && found {
		return true
	}
	return false
}

func readElixirVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".elixir_version", ".tool-versions", "mix.exs"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Elixir version hint from %s: %w", rel, err)
		}
		if version, ok := parseElixirVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseElixirVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".elixir_version":
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
			if len(fields) >= 2 && fields[0] == "elixir" {
				return fields[1], true
			}
		}
	case "mix.exs":
		inProject := false
		depth := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !inProject {
				if strings.HasPrefix(line, "def project") {
					inProject = true
					depth = strings.Count(line, "[") - strings.Count(line, "]")
					if strings.Contains(line, "elixir:") || strings.Contains(line, "elixir :") {
						if value := elixirMixProjectValue(line); value != "" {
							return value, true
						}
					}
					if depth <= 0 {
						continue
					}
				} else {
					continue
				}
			}
			if strings.Contains(line, "elixir:") || strings.Contains(line, "elixir :") {
				value := elixirMixProjectValue(line)
				if value != "" {
					return value, true
				}
			}
			depth += strings.Count(line, "[") - strings.Count(line, "]")
			if depth <= 0 {
				inProject = false
				depth = 0
			}
		}
	}
	return "", false
}

func elixirMixProjectValue(line string) string {
	for _, sep := range []string{"elixir:", "elixir :"} {
		idx := strings.Index(line, sep)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len(sep):])
		rest = strings.TrimRight(rest, ",]")
		rest = strings.TrimSpace(rest)
		rest = strings.Trim(rest, "\"'")
		if rest == "" {
			return ""
		}
		if c := rest[0]; c == '"' || c == '\'' {
			return ""
		}
		return rest
	}
	return ""
}

func (s *Scaffolder) resolveGoVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(goResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveDotnetVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(dotnetResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveNodeVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(nodeResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveRubyVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(rubyResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveRustVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(rustResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveJavaVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(javaResolver, repoRoot, selector, p)
}

func (s *Scaffolder) resolveElixirVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(elixirResolver, repoRoot, selector, p)
}

// versionResolver parameterises the shared tool-resolution algorithm with
// the inputs that differ across the four built-in tool resolvers (go, dotnet,
// node, python).
//
//   - ltsFromLatest, when non-nil, computes the lts selector from the latest
//     resolved version (Go and Python); when nil, "lts" is passed through to
//     mise (.NET and Node).
//   - passThroughValid, when non-nil, lets resolveMiseVersion return the
//     (already-normalised) selector as-is when both mise and the bundled
//     catalog miss (.NET and Node); when nil, the selector errors out (Go
//     and Python).
//   - requiresExactPin, when true, makes resolveMiseVersion discard any
//     mise result that does not pin a full major.minor.patch version
//     (e.g. "1.95" shorthand echoed back as "1.95" rather than the
//     resolved patch). This guarantees that the scaffolded Dockerfile
//     records an exact pin. Only the Rust preset opts in today.
type versionResolver struct {
	label            string
	miseTool         string
	hintReader       func(repoRoot string) (string, bool, error)
	normalize        func(selector string) string
	catalog          map[string]string
	ltsFromLatest    func(latestVersion string) (string, error)
	passThroughValid func(selector string) bool
	requiresExactPin bool
}

// goResolver is the versionResolver configuration for Go. The ltsFromLatest
// hook goes one minor back from the latest resolved version; the Go-specific
// quirk is that for Go 1.20 and older, the prefix is "prefix:MAJOR.MINOR"
// (a Go module-version pseudo-constraint that matches the latest patch of
// that minor).
var goResolver = versionResolver{
	label:      "Go",
	miseTool:   "go",
	hintReader: readGoVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if len(selector) > 2 && strings.HasPrefix(strings.ToLower(selector), "go") && selector[2] >= '0' && selector[2] <= '9' {
			return selector[2:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledGoVersionCatalog,
	ltsFromLatest: func(version string) (string, error) {
		version = strings.TrimSpace(version)
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
	},
}

// dotnetResolver is the versionResolver configuration for .NET. The
// passThroughValid hook returns the (already-normalised) selector as-is
// for any non-empty, non-"latest", non-"lts" selector that misses both
// mise and the bundled catalog.
var dotnetResolver = versionResolver{
	label:      ".NET SDK",
	miseTool:   "dotnet",
	hintReader: readDotnetVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if len(selector) > 6 && strings.HasPrefix(strings.ToLower(selector), "dotnet") && selector[6] >= '0' && selector[6] <= '9' {
			return selector[6:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledDotnetVersionCatalog,
	passThroughValid: func(selector string) bool {
		return selector != "" && !strings.EqualFold(selector, "latest") && !strings.EqualFold(selector, "lts")
	},
}

// nodeResolver is the versionResolver configuration for Node. The
// passThroughValid hook returns the (already-normalised) selector as-is
// when it matches nodeVersionSelectorPattern (a `\d+(?:\.\d+){0,2}`
// version shape) and is not "latest"/"lts".
var nodeResolver = versionResolver{
	label:      "Node",
	miseTool:   "node",
	hintReader: readNodeVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			return ""
		}
		lower := strings.ToLower(selector)
		switch {
		case lower == "repo", lower == "latest", lower == "lts":
			return lower
		case strings.HasPrefix(lower, "lts/"):
			return "lts"
		}
		if len(selector) > 1 && strings.HasPrefix(lower, "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		if version := nodeVersionSelectorPattern.FindString(selector); version != "" {
			return version
		}
		return selector
	},
	catalog: bundledNodeVersionCatalog,
	passThroughValid: func(selector string) bool {
		return selector != "" && nodeVersionSelectorPattern.MatchString(selector)
	},
}

// pythonResolver is the versionResolver configuration for Python. The
// ltsFromLatest hook computes the lts selector by going one minor back
// from the latest resolved version.
var pythonResolver = versionResolver{
	label:      "Python",
	miseTool:   "python",
	hintReader: readPythonVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if len(selector) > 6 && strings.HasPrefix(strings.ToLower(selector), "python") && selector[6] >= '0' && selector[6] <= '9' {
			return selector[6:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledPythonVersionCatalog,
	ltsFromLatest: func(version string) (string, error) {
		version = strings.TrimSpace(version)
		parts := strings.Split(version, ".")
		if len(parts) < 2 {
			return "", fmt.Errorf("unexpected Python version %q", version)
		}

		major, err := strconv.Atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("parse Python major version %q: %w", version, err)
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			return "", fmt.Errorf("parse Python minor version %q: %w", version, err)
		}
		if minor == 0 {
			return "", fmt.Errorf("unexpected Python version %q", version)
		}
		minor--

		return fmt.Sprintf("%d.%d", major, minor), nil
	},
}

// elixirResolver is the versionResolver configuration for Elixir. The
// normalize hook accepts mise-style selectors ("1.18", "v1.18", "~> 1.18")
// and the bare Elixir version form ("1.18.4"). We deliberately do not pass
// range selectors through when the bundled catalog misses: scaffolded
// Dockerfiles must pin an exact version or fail.
var elixirResolver = versionResolver{
	label:      "Elixir",
	miseTool:   "elixir",
	hintReader: readElixirVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if strings.HasPrefix(selector, "~>") {
			selector = strings.TrimSpace(strings.TrimPrefix(selector, "~>"))
		}
		if len(selector) > 6 && strings.HasPrefix(strings.ToLower(selector), "elixir") && selector[6] >= '0' && selector[6] <= '9' {
			return selector[6:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledElixirVersionCatalog,
	passThroughValid: func(selector string) bool {
		selector = strings.TrimSpace(selector)
		if selector == "" || strings.EqualFold(selector, "latest") || strings.EqualFold(selector, "lts") {
			return false
		}
		if strings.HasPrefix(selector, "~>") {
			return false
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			selector = selector[1:]
		}
		parts := strings.Split(selector, "-")
		versionParts := strings.Split(parts[0], ".")
		if len(versionParts) != 3 {
			return false
		}
		for _, part := range versionParts {
			if part == "" {
				return false
			}
			for _, r := range part {
				if r < '0' || r > '9' {
					return false
				}
			}
		}
		return true
	},
}

// rubyResolver is the versionResolver configuration for Ruby. The
// ltsFromLatest hook computes the lts selector by going one minor back
// from the latest resolved version.
var rubyResolver = versionResolver{
	label:      "Ruby",
	miseTool:   "ruby",
	hintReader: readRubyVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if len(selector) > 4 && strings.HasPrefix(strings.ToLower(selector), "ruby") && selector[4] >= '0' && selector[4] <= '9' {
			return selector[4:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledRubyVersionCatalog,
	ltsFromLatest: func(version string) (string, error) {
		version = strings.TrimSpace(version)
		parts := strings.Split(version, ".")
		if len(parts) < 2 {
			return "", fmt.Errorf("unexpected Ruby version %q", version)
		}

		major, err := strconv.Atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("parse Ruby major version %q: %w", version, err)
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			return "", fmt.Errorf("parse Ruby minor version %q: %w", version, err)
		}
		if minor == 0 {
			return "", fmt.Errorf("unexpected Ruby version %q", version)
		}
		minor--

		return fmt.Sprintf("%d.%d", major, minor), nil
	},
	passThroughValid: func(selector string) bool {
		selector = strings.TrimSpace(selector)
		if selector == "" || strings.EqualFold(selector, "latest") || strings.EqualFold(selector, "lts") {
			return false
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			selector = selector[1:]
		}
		parts := strings.Split(selector, ".")
		if len(parts) < 2 || len(parts) > 3 {
			return false
		}
		for _, part := range parts {
			if part == "" {
				return false
			}
			for _, r := range part {
				if r < '0' || r > '9' {
					return false
				}
			}
		}
		return true
	},
}

// rustResolver is the versionResolver configuration for Rust. The normalize
// hook accepts mise-style selectors ("1.77", "v1.77", "rust1.77") and the
// repo hint forms supported by readRustVersionHint. requiresExactPin is
// enabled so the scaffolded Dockerfile always records a full
// major.minor.patch version rather than a shorthand the mise shim
// echoed back unresolved.
var rustResolver = versionResolver{
	label:            "Rust",
	miseTool:         "rust",
	hintReader:       readRustVersionHint,
	requiresExactPin: true,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		if len(selector) > 4 && strings.HasPrefix(strings.ToLower(selector), "rust") && selector[4] >= '0' && selector[4] <= '9' {
			return selector[4:]
		}
		if len(selector) > 1 && strings.HasPrefix(strings.ToLower(selector), "v") && selector[1] >= '0' && selector[1] <= '9' {
			return selector[1:]
		}
		return selector
	},
	catalog: bundledRustVersionCatalog,
	passThroughValid: func(selector string) bool {
		return selector != "" && rustVersionSelectorPattern.MatchString(selector)
	},
}

// javaResolver is the versionResolver configuration for Java. The ltsFromLatest
// hook goes one major back from the latest resolved version, mirroring Ruby's
// pattern. Java LTS releases are every 3 years (8, 11, 17, 21), so for the
// current `latest=21` the lts selector resolves to 17. The normalize hook
// strips common JDK identifier prefixes ("java", "jdk", "openjdk") so users
// can pass `jdk21` or `openjdk-21` and still hit the catalog.
var javaResolver = versionResolver{
	label:      "Java",
	miseTool:   "java",
	hintReader: readJavaVersionHint,
	normalize: func(selector string) string {
		selector = strings.TrimSpace(selector)
		lower := strings.ToLower(selector)
		if strings.HasPrefix(lower, "openjdk-") {
			return selector[len("openjdk-"):]
		}
		if strings.HasPrefix(lower, "jdk") && len(selector) > 3 && selector[3] >= '0' && selector[3] <= '9' {
			return selector[3:]
		}
		if strings.HasPrefix(lower, "java") && len(selector) > 4 && selector[4] >= '0' && selector[4] <= '9' {
			return selector[4:]
		}
		return selector
	},
	catalog: bundledJavaVersionCatalog,
	ltsFromLatest: func(version string) (string, error) {
		version = strings.TrimSpace(version)
		parts := strings.Split(version, ".")
		if len(parts) == 0 {
			return "", fmt.Errorf("unexpected Java version %q", version)
		}
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("parse Java major version %q: %w", version, err)
		}
		if major <= 1 {
			return "", fmt.Errorf("unexpected Java major version %q", version)
		}
		// Java LTS releases land every 3 majors (8, 11, 17, 21, ...).
		// Walk back at most 6 majors to find an LTS that the bundled catalog
		// actually has a pin for; if none found in that window, fall back to
		// the lowest cataloged major.
		for offset := 1; offset <= 6; offset++ {
			candidate := major - offset
			if candidate < 1 {
				break
			}
			key := fmt.Sprintf("%d", candidate)
			if _, ok := bundledJavaVersionCatalog[key]; ok {
				return key, nil
			}
		}
		return "", fmt.Errorf("no Java LTS version found in bundled catalog for latest=%q", version)
	},
	passThroughValid: func(selector string) bool {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			return false
		}
		return javaVersionSelectorPattern.MatchString(selector)
	},
}

// resolveVersion resolves a version for a single tool, reading the repo hint,
// determining the choice via Prompter or default, and dispatching to the
// mise-backed resolution path.
func resolveVersion(r versionResolver, repoRoot, selector string, p Prompter) (string, error) {
	hint, found, err := r.hintReader(repoRoot)
	if err != nil {
		return "", err
	}

	choice := strings.TrimSpace(selector)
	if choice == "repo" && !found {
		choice = ""
	}
	if choice == "" {
		if found {
			if p != nil {
				selected, err := p.Select(fmt.Sprintf("Choose a %s version (repo: %s):", r.label, hint), []string{"repo", "latest", "lts"})
				if err == nil {
					choice = r.normalize(selected)
				}
			}
			if choice == "" {
				choice = "repo"
			}
		} else {
			if p != nil {
				selected, err := p.Select(fmt.Sprintf("Choose a %s version:", r.label), []string{"latest", "lts"})
				if err == nil {
					choice = r.normalize(selected)
				}
			}
			if choice == "" {
				choice = "latest"
			}
		}
	}

	resolved, err := resolveMiseVersionChoice(r, choice, hint, found)
	if err != nil {
		return "", fmt.Errorf("resolve %s version: %w", r.miseTool, err)
	}
	return resolved, nil
}

// resolveMiseVersionChoice normalizes the choice and dispatches to
// resolveMiseVersion. The "lts" branch either resolves "latest" first and then
// the ltsFromLatest-computed selector, or passes "lts" through to mise.
func resolveMiseVersionChoice(r versionResolver, choice, hint string, hintFound bool) (string, error) {
	choice = r.normalize(choice)
	if choice == "" {
		return "", fmt.Errorf("empty version selector")
	}

	switch strings.ToLower(choice) {
	case "repo":
		if !hintFound {
			return "", fmt.Errorf("no repo %s version hint found", r.label)
		}
		return resolveMiseVersion(r, r.normalize(hint))
	case "latest":
		return resolveMiseVersion(r, "latest")
	case "lts":
		if r.ltsFromLatest == nil {
			return resolveMiseVersion(r, "lts")
		}
		latest, err := resolveMiseVersion(r, "latest")
		if err != nil {
			return "", err
		}
		prefix, err := r.ltsFromLatest(latest)
		if err != nil {
			return "", err
		}
		return resolveMiseVersion(r, prefix)
	}

	return resolveMiseVersion(r, choice)
}

// resolveMiseVersion calls `mise latest <miseTool>[@selector]` and falls back
// to the resolver's bundled catalog for "", "latest", "lts", and the exact
// selector. When the resolver's passThroughValid hook accepts the selector
// (.NET and Node), the selector is returned as-is. Selectors that are neither
// in the catalog nor recognised by mise and not accepted by the hook return
// an error.
func resolveMiseVersion(r versionResolver, selector string) (string, error) {
	selector = r.normalize(selector)
	args := []string{"latest"}
	switch strings.ToLower(selector) {
	case "", "latest":
		args = append(args, r.miseTool)
	case "lts":
		args = append(args, r.miseTool+"@lts")
	default:
		args = append(args, r.miseTool+"@"+selector)
	}

	cmd := exec.Command("mise", args...)
	out, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(out))
		if version != "" {
			if r.requiresExactPin && version == selector {
				parts := strings.Split(version, ".")
				if len(parts) < 3 {
					version = ""
				}
			}
			if version != "" {
				return version, nil
			}
		}
	}

	if version, ok := r.catalog[selector]; ok {
		return version, nil
	}
	if selector == "" || strings.EqualFold(selector, "latest") {
		if version, ok := r.catalog["latest"]; ok {
			return version, nil
		}
	}
	if strings.EqualFold(selector, "lts") {
		if version, ok := r.catalog["lts"]; ok {
			return version, nil
		}
	}
	if r.passThroughValid != nil && r.passThroughValid(selector) {
		return selector, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s version %q: %w", r.miseTool, selector, err)
	}
	return "", fmt.Errorf("resolve %s version %q: mise returned empty output and no bundled fallback", r.miseTool, selector)
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
	if choice == "repo" {
		choice = ""
	}
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

func (s *Scaffolder) renderBuildToolsDockerfile(preset BuildToolsPreset, defaultAgent, goVersion, dotnetVersion, nodeVersion, pythonVersion, elixirVersion, erlangVersion, rubyVersion, rustVersion, javaVersion string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# sandman build-tools: %s\n", preset.Name)
	fmt.Fprintf(&out, "# sandman default-agent: %s\n", defaultAgent)
	fmt.Fprintf(&out, "# sandman installed-agents: opencode\n")
	if preset.Name == goBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman go-version: %s\n", goVersion)
	}
	if preset.Name == dotnetBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman dotnet-version: %s\n", dotnetVersion)
	}
	if preset.Name == nodeBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman node-version: %s\n", nodeVersion)
	}
	if preset.Name == pythonBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman python-version: %s\n", pythonVersion)
	}
	if preset.Name == elixirBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman elixir-version: %s\n", elixirVersion)
		fmt.Fprintf(&out, "# sandman erlang-version: %s\n", erlangVersion)
	}
	if preset.Name == rubyBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman ruby-version: %s\n", rubyVersion)
	}
	if preset.Name == rustBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman rust-version: %s\n", rustVersion)
	}
	if preset.Name == javaBuildToolsPreset {
		fmt.Fprintf(&out, "# sandman java-version: %s\n", javaVersion)
	}
	fmt.Fprintf(&out, "# sandman mise-version: %s\n", preset.MiseVersion)
	fmt.Fprintf(&out, "# sandman rtk-version: %s\n", DefaultRTKVersion)
	fmt.Fprintf(&out, "FROM %s\n", preset.BaseImage)
	aptPackages := append([]string{}, preset.SharedPackages...)
	aptPackages = append(aptPackages, preset.ExtraPackages...)
	fmt.Fprintf(&out, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(aptPackages, " "))
	fmt.Fprintf(&out, "RUN MISE_VERSION=%s curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh\n", preset.MiseVersion)
	out.WriteString("ENV MISE_GLOBAL_CONFIG_FILE=\"/etc/mise/config.toml\"\n")
	out.WriteString("ENV MISE_CONFIG_DIR=\"/etc/mise\"\n")
	out.WriteString("ENV MISE_DATA_DIR=\"/usr/local/share/mise\"\n")
	out.WriteString("ENV MISE_STATE_DIR=\"/usr/local/share/mise/state\"\n")
	out.WriteString("ENV MISE_CACHE_DIR=\"/usr/local/share/mise/cache\"\n")
	out.WriteString("ENV PATH=\"/usr/local/share/mise/shims:/usr/local/share/mise/bin:$PATH\"\n")
	out.WriteString("WORKDIR /app\n")
	if preset.Name == goBuildToolsPreset {
		out.WriteString("ENV GOPATH=\"/.local/share/go\"\n")
		out.WriteString("ENV GOMODCACHE=\"/.cache/go/pkg/mod\"\n")
		out.WriteString(renderGoInstallCommand(goVersion))
	}
	if preset.Name == dotnetBuildToolsPreset {
		out.WriteString(renderDotnetInstallCommand(dotnetVersion))
	}
	if preset.Name == nodeBuildToolsPreset {
		out.WriteString(renderNodeInstallCommand(nodeVersion))
	}
	if preset.Name == pythonBuildToolsPreset {
		out.WriteString(renderPythonInstallCommand(pythonVersion))
	}
	if preset.Name == elixirBuildToolsPreset {
		out.WriteString(renderElixirInstallCommand(elixirVersion, erlangVersion))
	}
	if preset.Name == rubyBuildToolsPreset {
		out.WriteString(renderRubyInstallCommand(rubyVersion))
	}
	if preset.Name == rustBuildToolsPreset {
		out.WriteString(renderRustInstallCommand(rustVersion))
	}
	if preset.Name == javaBuildToolsPreset {
		out.WriteString(renderJavaInstallCommand(javaVersion))
	}
	out.WriteString(renderCodeindexInstallCommand())
	out.WriteString(renderAgentInstallCommand("opencode", DefaultBuiltInAgentVersion("opencode")))
	out.WriteString(renderRTKInstallCommand())
	return out.String()
}

func (s *Scaffolder) resolvePythonVersion(repoRoot, selector string, p Prompter) (string, error) {
	return resolveVersion(pythonResolver, repoRoot, selector, p)
}

func readNodeVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".node-version", ".nvmrc", ".tool-versions", "package.json"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Node version hint from %s: %w", rel, err)
		}
		if version, ok := parseNodeVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func readDotnetVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{"global.json", "Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read .NET version hint from %s: %w", rel, err)
		}
		if version, ok := parseDotnetVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parseDotnetVersionHint(name string, data []byte) (string, bool) {
	if name == "global.json" {
		type globalJSON struct {
			SDK struct {
				Version string `json:"version"`
			} `json:"sdk"`
		}
		var doc globalJSON
		if err := json.Unmarshal(data, &doc); err == nil {
			if version := strings.TrimSpace(doc.SDK.Version); version != "" {
				return version, true
			}
		}
	}
	return "", false
}

func parseNodeVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".node-version", ".nvmrc":
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
			if len(fields) >= 2 && (fields[0] == "node" || fields[0] == "nodejs") {
				return fields[1], true
			}
		}
	case "package.json":
		type packageJSON struct {
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
			Volta struct {
				Node string `json:"node"`
			} `json:"volta"`
		}
		var pkg packageJSON
		if err := json.Unmarshal(data, &pkg); err == nil {
			if version := strings.TrimSpace(pkg.Volta.Node); version != "" {
				return version, true
			}
			if version := strings.TrimSpace(pkg.Engines.Node); version != "" {
				return version, true
			}
		}
	}
	return "", false
}

func readPythonVersionHint(repoRoot string) (string, bool, error) {
	for _, rel := range []string{".python-version", "pyproject.toml", ".tool-versions", "setup.cfg", "setup.py"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("read Python version hint from %s: %w", rel, err)
		}
		if version, ok := parsePythonVersionHint(rel, data); ok {
			return version, true, nil
		}
	}
	return "", false, nil
}

func parsePythonVersionHint(name string, data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	switch name {
	case ".python-version":
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
			if len(fields) >= 2 && fields[0] == "python" {
				return fields[1], true
			}
		}
	default:
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			lowerLine := strings.ToLower(line)
			if strings.Contains(lowerLine, "requires-python") || strings.Contains(lowerLine, "python_requires") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) >= 2 {
					value := strings.TrimSpace(parts[1])
					value = strings.Trim(value, "\"' ")
					for _, c := range []string{",", ">", "<", "=", "!"} {
						value = strings.ReplaceAll(value, c, " ")
					}
					fields := strings.Fields(value)
					for _, f := range fields {
						f = strings.TrimSpace(f)
						if f != "" && f[0] >= '0' && f[0] <= '9' {
							return f, true
						}
					}
				}
			}
		}
	}
	return "", false
}

func renderGoInstallCommand(version string) string {
	return fmt.Sprintf("RUN mise use -g --pin go@%s\n", version)
}

func renderDotnetInstallCommand(version string) string {
	return fmt.Sprintf("RUN mise use -g --pin dotnet@%s\n", version)
}

func renderNodeInstallCommand(version string) string {
	var out strings.Builder
	out.WriteString(fmt.Sprintf("RUN mise use -g --pin node@%s\n", version))
	out.WriteString("RUN corepack enable\n")
	return out.String()
}

func renderPythonInstallCommand(version string) string {
	var out strings.Builder
	out.WriteString(fmt.Sprintf("RUN mise use -g --pin python@%s\n", version))
	out.WriteString("RUN pip3 install uv\n")
	return out.String()
}

// deriveErlangOTPFromElixir returns the matching Erlang/OTP major release
// for the given Elixir version. When the version string includes the
// `-otp-<NN>` suffix (mise's canonical Elixir version form), the OTP is
// extracted from there. Otherwise the bundled Elixir-OTP table is
// consulted by major.minor prefix, falling back to the catalog default.
func deriveErlangOTPFromElixir(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return bundledElixirDefaultOTP, nil
	}
	if idx := strings.Index(version, "-otp-"); idx >= 0 {
		otp := strings.TrimSpace(version[idx+len("-otp-"):])
		if otp != "" {
			otp = strings.TrimFunc(otp, func(r rune) bool {
				return r < '0' || r > '9'
			})
			if otp != "" {
				return otp, nil
			}
		}
	}
	if match := elixirVersionPattern.FindStringSubmatch(version); len(match) == 3 {
		key := match[1] + "." + match[2]
		if otp, ok := bundledElixirOTPMap[key]; ok {
			return otp, nil
		}
	}
	return bundledElixirDefaultOTP, nil
}

// renderElixirInstallCommand returns the Dockerfile RUN lines for pinning
// erlang + elixir via mise and installing the mainstream companion
// tooling (hex, rebar3). The elixir line uses the full pinned string
// (which keeps the `-otp-<NN>` suffix so the mise shim is unambiguous).
func renderElixirInstallCommand(elixirVersion, otpVersion string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "RUN mise use -g --pin erlang@%s\n", otpVersion)
	fmt.Fprintf(&out, "RUN mise use -g --pin elixir@%s\n", elixirVersion)
	out.WriteString("RUN mix local.hex --force\n")
	out.WriteString("RUN mix local.rebar --force\n")
	return out.String()
}

func renderRubyInstallCommand(version string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "RUN mise use -g --pin ruby@%s\n", version)
	out.WriteString("RUN gem install bundler\n")
	return out.String()
}

func renderRustInstallCommand(version string) string {
	return fmt.Sprintf("RUN mise use -g --pin rust@%s\n", version)
}

func renderJavaInstallCommand(version string) string {
	return fmt.Sprintf("RUN mise use -g --pin java@%s\n", version)
}

func renderCodeindexInstallCommand() string {
	return "RUN git clone https://github.com/rafaelromao/codeindex /tmp/codeindex && pip3 install -e /tmp/codeindex --break-system-packages\n"
}

func renderAgentInstallCommand(agent, version string) string {
	switch agent {
	case "opencode":
		return fmt.Sprintf("RUN npm install -g opencode-ai@%s\n", version)
	default:
		return ""
	}
}

func renderRTKInstallCommand() string {
	return fmt.Sprintf("RUN curl -fsSL https://github.com/rtk-ai/rtk/releases/download/%s/rtk-x86_64-unknown-linux-musl.tar.gz | tar -xz -C /usr/local/bin\n", DefaultRTKVersion)
}

// ValidateDockerfileMetadata fails when scaffold metadata drift is detected.
// Metadata-free Dockerfiles are treated as opaque custom files.
// tool-version and mise-version are intentionally not validated here because
// runtime config has no canonical pinned value to compare against.
func ValidateDockerfileMetadata(repoRoot, expectedBuildTools, expectedDefaultAgent string) error {
	if strings.TrimSpace(expectedBuildTools) == "" {
		expectedBuildTools = defaultBuildToolsPreset
	}
	if strings.TrimSpace(expectedDefaultAgent) == "" {
		expectedDefaultAgent = config.DefaultAgent
	}
	dockerfilePath := paths.NewLayout(&config.Config{}, repoRoot).DockerfilePath()
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
	if meta.DefaultAgent != expectedDefaultAgent {
		return fmt.Errorf("scaffold metadata drift: Dockerfile default-agent %q does not match config default agent %q", meta.DefaultAgent, expectedDefaultAgent)
	}
	wantAgents := []string{"opencode"}
	if !reflect.DeepEqual(meta.InstalledAgents, wantAgents) {
		return fmt.Errorf("scaffold metadata drift: Dockerfile installed-agents %v does not match expected %v", meta.InstalledAgents, wantAgents)
	}
	return nil
}

type dockerfileMetadata struct {
	BuildToolsPreset string
	DefaultAgent     string
	InstalledAgents  []string
	GoVersion        string
	NodeVersion      string
	PythonVersion    string
	ElixirVersion    string
	ErlangVersion    string
	RubyVersion      string
	RustVersion      string
	JavaVersion      string
	ToolVersion      string
	MiseVersion      string
	RtkVersion       string
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
		case "default-agent":
			meta.DefaultAgent = strings.TrimSpace(value)
		case "installed-agents":
			parts := strings.Split(strings.TrimSpace(value), ",")
			meta.InstalledAgents = meta.InstalledAgents[:0]
			for _, part := range parts {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					meta.InstalledAgents = append(meta.InstalledAgents, trimmed)
				}
			}
		case "tool-version":
			meta.ToolVersion = strings.TrimSpace(value)
		case "go-version":
			meta.GoVersion = strings.TrimSpace(value)
		case "node-version":
			meta.NodeVersion = strings.TrimSpace(value)
		case "python-version":
			meta.PythonVersion = strings.TrimSpace(value)
		case "elixir-version":
			meta.ElixirVersion = strings.TrimSpace(value)
		case "erlang-version":
			meta.ErlangVersion = strings.TrimSpace(value)
		case "ruby-version":
			meta.RubyVersion = strings.TrimSpace(value)
		case "rust-version":
			meta.RustVersion = strings.TrimSpace(value)
		case "java-version":
			meta.JavaVersion = strings.TrimSpace(value)
		case "mise-version":
			meta.MiseVersion = strings.TrimSpace(value)
		case "rtk-version":
			meta.RtkVersion = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return dockerfileMetadata{}, false, fmt.Errorf("scan Dockerfile metadata: %w", err)
	}
	return meta, found, nil
}
