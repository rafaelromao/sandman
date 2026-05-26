package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

type portalLaunchFormData struct {
	LaunchModeOptionsHTML    template.HTML
	SelectionModeOptionsHTML template.HTML
	AgentOptionsHTML         template.HTML
	SandboxOptionsHTML       template.HTML
	Issues                   string
	Label                    string
	Query                    string
	Next                     int
	IncludeDependencies      bool
	Prompt                   string
	Template                 string
	ReviewCommand            string
	PromptArgs               string
	Agent                    string
	Model                    string
	BaseBranch               string
	Parallel                 int
	StartDelay               int
	ContainerCapacity        int
	MaxContainers            int
	Sandbox                  string
}

type portalLaunchRequest struct {
	LaunchMode          string `json:"launchMode"`
	SelectionMode       string `json:"selectionMode"`
	Issues              string `json:"issues"`
	Label               string `json:"label"`
	Query               string `json:"query"`
	Next                *int   `json:"next"`
	IncludeDependencies bool   `json:"includeDependencies"`
	Prompt              string `json:"prompt"`
	Template            string `json:"template"`
	ReviewCommand       string `json:"reviewCommand"`
	PromptArgs          string `json:"promptArgs"`
	Agent               string `json:"agent"`
	Model               string `json:"model"`
	BaseBranch          string `json:"baseBranch"`
	Parallel            *int   `json:"parallel"`
	StartDelay          *int   `json:"startDelay"`
	ContainerCapacity   *int   `json:"containerCapacity"`
	MaxContainers       *int   `json:"maxContainers"`
	Sandbox             string `json:"sandbox"`
}

type portalLaunchResponse struct {
	Message string   `json:"message"`
	Args    []string `json:"args"`
}

type portalUnifiedLaunchRequest struct {
	Command             string `json:"command"`
	LaunchMode          string `json:"launchMode"`
	SelectionMode       string `json:"selectionMode"`
	Issues              string `json:"issues"`
	Issue               int    `json:"issue,omitempty"`
	Label               string `json:"label"`
	Query               string `json:"query"`
	Next                *int   `json:"next"`
	IncludeDependencies bool   `json:"includeDependencies"`
	Prompt              string `json:"prompt"`
	Template            string `json:"template"`
	ReviewCommand       string `json:"reviewCommand"`
	PromptArgs          string `json:"promptArgs"`
	Agent               string `json:"agent"`
	Model               string `json:"model"`
	BaseBranch          string `json:"baseBranch"`
	Parallel            *int   `json:"parallel"`
	StartDelay          *int   `json:"startDelay"`
	ContainerCapacity   *int   `json:"containerCapacity"`
	MaxContainers       *int   `json:"maxContainers"`
	Sandbox             string `json:"sandbox"`
	CleanMode           string `json:"cleanMode,omitempty"`
	Confirmed           bool   `json:"confirmed,omitempty"`
	ConfigMode          string `json:"configMode,omitempty"`
	ConfigKey           string `json:"configKey,omitempty"`
	ConfigValue         string `json:"configValue,omitempty"`
}

type portalOption struct {
	Value    string
	Label    string
	Selected bool
}

var portalStartRun = startPortalRun

func loadPortalLaunchConfig(store config.Store) (*config.Config, error) {
	if store == nil {
		return nil, nil
	}
	cfg, err := store.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return cfg, nil
}

func portalLaunchDataFromConfig(cfg *config.Config) portalLaunchFormData {
	if cfg == nil {
		cfg = &config.Config{}
	}

	agentName := strings.TrimSpace(cfg.DefaultAgent)
	if agentName == "" {
		agentName = strings.TrimSpace(cfg.Agent)
	}
	if agentName == "" {
		agentName = config.DefaultAgent
	}

	reviewCommand := cfg.EffectiveReviewCommand()
	baseBranch := strings.TrimSpace(cfg.Git.BaseBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	parallel := cfg.DefaultParallel
	if parallel <= 0 {
		parallel = config.DefaultParallel
	}
	startDelay := cfg.StartDelay
	if startDelay < 0 {
		startDelay = config.DefaultStartDelay
	}
	containerCapacity := cfg.ContainerCapacity
	if containerCapacity < 0 {
		containerCapacity = config.DefaultContainerCapacity
	}
	maxContainers := cfg.MaxContainers
	if maxContainers < 0 {
		maxContainers = config.DefaultMaxContainers
	}
	sandbox := strings.TrimSpace(cfg.Sandbox)
	if sandbox == "" {
		sandbox = config.DefaultSandbox
	}

	agentOptions := portalAgentOptions(cfg, agentName)

	selectedAgent, _ := cfg.ResolveAgentProvider(agentName)
	model := strings.TrimSpace(selectedAgent.Model)

	return portalLaunchFormData{
		LaunchModeOptionsHTML:    portalRadioOptionsHTML("launch-mode", []portalOption{{Value: "issue-driven", Label: "Issue-driven", Selected: true}, {Value: "prompt-only", Label: "Prompt-only"}}, "issue-driven"),
		SelectionModeOptionsHTML: portalRadioOptionsHTML("selection-mode", []portalOption{{Value: "issues", Label: "Issue numbers", Selected: true}, {Value: "label", Label: "Label"}, {Value: "query", Label: "Query"}, {Value: "next", Label: "Next ready issue"}}, "issues"),
		AgentOptionsHTML:         agentOptions,
		SandboxOptionsHTML:       portalSelectOptionsHTML([]portalOption{{Value: "podman", Label: "podman", Selected: sandbox == "podman"}, {Value: "docker", Label: "docker", Selected: sandbox == "docker"}, {Value: "worktree", Label: "worktree", Selected: sandbox == "worktree"}}, sandbox),
		Next:                     1,
		ReviewCommand:            reviewCommand,
		Agent:                    agentName,
		Model:                    model,
		BaseBranch:               baseBranch,
		Parallel:                 parallel,
		StartDelay:               startDelay,
		ContainerCapacity:        containerCapacity,
		MaxContainers:            maxContainers,
		Sandbox:                  sandbox,
	}
}

func portalAgentOptions(cfg *config.Config, selected string) template.HTML {
	providers := make(map[string]config.Agent)
	if cfg != nil && len(cfg.AgentProviders) > 0 {
		providers = cfg.AgentProviders
	} else {
		for name, preset := range config.BuiltInAgentPresets {
			providers[name] = preset.Agent(name)
		}
	}

	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		iBuiltin := portalBuiltinAgentOrder(names[i])
		jBuiltin := portalBuiltinAgentOrder(names[j])
		if iBuiltin != jBuiltin {
			if iBuiltin < 0 {
				return false
			}
			if jBuiltin < 0 {
				return true
			}
			return iBuiltin < jBuiltin
		}
		return strings.Compare(names[i], names[j]) < 0
	})

	options := make([]portalOption, 0, len(names))
	for _, name := range names {
		label := name
		if preset, ok := config.BuiltInAgentPresets[name]; ok {
			label = preset.DisplayName
		}
		options = append(options, portalOption{Value: name, Label: label, Selected: name == selected})
	}
	if len(options) == 0 {
		return ""
	}
	return portalSelectOptionsHTML(options, selected)
}

func portalBuiltinAgentOrder(name string) int {
	switch name {
	case "opencode":
		return 0
	case "pi":
		return 1
	default:
		return -1
	}
}

func portalSelectOptionsHTML(options []portalOption, selected string) template.HTML {
	var out strings.Builder
	for _, option := range options {
		isSelected := option.Selected || option.Value == selected
		fmt.Fprintf(&out, "<option value=\"%s\"%s>%s</option>\n", template.HTMLEscapeString(option.Value), portalSelectedAttr(isSelected), template.HTMLEscapeString(option.Label))
	}
	return template.HTML(out.String())
}

func portalRadioOptionsHTML(name string, options []portalOption, selected string) template.HTML {
	var out strings.Builder
	for _, option := range options {
		isSelected := option.Selected || option.Value == selected
		fmt.Fprintf(&out, "<label class=\"launch-radio\"><input type=\"radio\" name=\"%s\" value=\"%s\"%s> %s</label>\n", template.HTMLEscapeString(name), template.HTMLEscapeString(option.Value), portalCheckedAttr(isSelected), template.HTMLEscapeString(option.Label))
	}
	return template.HTML(out.String())
}

func portalCheckedAttr(checked bool) string {
	if checked {
		return " checked"
	}
	return ""
}

func parsePortalUnifiedLaunchRequest(r *http.Request) (portalUnifiedLaunchRequest, error) {
	var req portalUnifiedLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return portalUnifiedLaunchRequest{}, fmt.Errorf("parse launch request: %w", err)
	}
	return req, nil
}

func (req portalUnifiedLaunchRequest) runRequest() portalLaunchRequest {
	return portalLaunchRequest{
		LaunchMode:          req.LaunchMode,
		SelectionMode:       req.SelectionMode,
		Issues:              req.Issues,
		Label:               req.Label,
		Query:               req.Query,
		Next:                req.Next,
		IncludeDependencies: req.IncludeDependencies,
		Prompt:              req.Prompt,
		Template:            req.Template,
		ReviewCommand:       req.ReviewCommand,
		PromptArgs:          req.PromptArgs,
		Agent:               req.Agent,
		Model:               req.Model,
		BaseBranch:          req.BaseBranch,
		Parallel:            req.Parallel,
		StartDelay:          req.StartDelay,
		ContainerCapacity:   req.ContainerCapacity,
		MaxContainers:       req.MaxContainers,
		Sandbox:             req.Sandbox,
	}
}

func (req portalUnifiedLaunchRequest) commandRequest() portalCommandLaunchRequest {
	return portalCommandLaunchRequest{
		Preset:      req.Command,
		Issue:       req.Issue,
		Prompt:      req.Prompt,
		CleanMode:   req.CleanMode,
		Confirmed:   req.Confirmed,
		ConfigMode:  req.ConfigMode,
		ConfigKey:   req.ConfigKey,
		ConfigValue: req.ConfigValue,
	}
}

func portalSelectedAttr(selected bool) string {
	if selected {
		return " selected"
	}
	return ""
}

func parsePortalLaunchRequest(r *http.Request) (portalLaunchRequest, error) {
	var req portalLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return portalLaunchRequest{}, fmt.Errorf("parse launch request: %w", err)
	}
	return req, nil
}

func buildPortalRunArgs(repoRoot string, cfg *config.Config, req portalLaunchRequest) ([]string, error) {
	launchMode := strings.TrimSpace(req.LaunchMode)
	if launchMode == "" {
		launchMode = "issue-driven"
	}
	selectionMode := strings.TrimSpace(req.SelectionMode)
	if selectionMode == "" {
		selectionMode = "issues"
	}

	reviewCommand := strings.TrimSpace(req.ReviewCommand)
	if reviewCommand == "" && cfg != nil {
		reviewCommand = cfg.EffectiveReviewCommand()
	}
	if reviewCommand == "" {
		reviewCommand = config.DefaultReviewCommand
	}

	agent := strings.TrimSpace(req.Agent)
	if agent == "" && cfg != nil {
		agent = strings.TrimSpace(cfg.DefaultAgent)
	}
	if agent == "" && cfg != nil {
		agent = strings.TrimSpace(cfg.Agent)
	}
	if agent == "" {
		agent = config.DefaultAgent
	}

	model := strings.TrimSpace(req.Model)
	if model == "" && cfg != nil {
		if resolved, err := cfg.ResolveAgentProvider(agent); err == nil {
			model = strings.TrimSpace(resolved.Model)
		}
	}

	baseBranch := strings.TrimSpace(req.BaseBranch)
	if baseBranch == "" && cfg != nil {
		baseBranch = strings.TrimSpace(cfg.Git.BaseBranch)
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	parallel := config.DefaultParallel
	if cfg != nil && cfg.DefaultParallel > 0 {
		parallel = cfg.DefaultParallel
	}
	if req.Parallel != nil {
		parallel = *req.Parallel
	}
	if parallel < 0 {
		return nil, fmt.Errorf("parallel must be 0 or greater")
	}

	startDelay := config.DefaultStartDelay
	if cfg != nil && cfg.StartDelay >= 0 {
		startDelay = cfg.StartDelay
	}
	if req.StartDelay != nil {
		startDelay = *req.StartDelay
	}
	if startDelay < 0 {
		return nil, fmt.Errorf("start_delay must be 0 or greater")
	}

	containerCapacity := config.DefaultContainerCapacity
	if cfg != nil && cfg.ContainerCapacity >= 0 {
		containerCapacity = cfg.ContainerCapacity
	}
	if req.ContainerCapacity != nil {
		containerCapacity = *req.ContainerCapacity
	}
	if containerCapacity < 0 {
		return nil, fmt.Errorf("container_capacity must be 0 or greater")
	}

	maxContainers := config.DefaultMaxContainers
	if cfg != nil && cfg.MaxContainers >= 0 {
		maxContainers = cfg.MaxContainers
	}
	if req.MaxContainers != nil {
		maxContainers = *req.MaxContainers
	}
	if maxContainers < 0 {
		return nil, fmt.Errorf("max_containers must be 0 or greater")
	}

	sandbox := strings.TrimSpace(req.Sandbox)
	if sandbox == "" && cfg != nil {
		sandbox = strings.TrimSpace(cfg.Sandbox)
	}
	if sandbox == "" {
		sandbox = config.DefaultSandbox
	}

	promptText := strings.TrimSpace(req.Prompt)
	templateText := strings.TrimSpace(req.Template)
	promptArgs := parsePortalPromptArgs(req.PromptArgs)
	promptRender := prompt.RenderConfig{ReviewCommand: reviewCommand, PromptArgs: promptArgsMap(promptArgs)}

	selectedPrompt := ""
	if promptText != "" {
		selectedPrompt = promptText
	} else if templateText != "" {
		content, err := readPortalTemplate(repoRoot, templateText)
		if err != nil {
			return nil, err
		}
		selectedPrompt = content
	}

	if launchMode == "prompt-only" {
		if selectedPrompt == "" {
			return nil, fmt.Errorf("prompt-only mode requires --prompt or --template")
		}
		if promptRequiresIssueSelection(prompt.ApplySubstitutions(selectedPrompt, promptRender)) {
			return nil, fmt.Errorf("prompt requires issue selection but no issue selection was provided")
		}
		selectionMode = ""
	}

	args := []string{"run"}
	if promptText != "" {
		args = append(args, "--prompt", promptText)
	}
	if templateText != "" {
		args = append(args, "--template", templateText)
	}
	if reviewCommand != "" {
		args = append(args, "--review-command", reviewCommand)
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if baseBranch != "" {
		args = append(args, "--base-branch", baseBranch)
	}
	if req.Parallel != nil || parallel > 0 {
		args = append(args, "--parallel", strconv.Itoa(parallel))
	}
	if req.StartDelay != nil || startDelay > 0 {
		args = append(args, "--start-delay", strconv.Itoa(startDelay))
	}
	if sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	if req.ContainerCapacity != nil || containerCapacity > 0 {
		args = append(args, "--container-capacity", strconv.Itoa(containerCapacity))
	}
	if req.MaxContainers != nil || maxContainers > 0 {
		args = append(args, "--max-containers", strconv.Itoa(maxContainers))
	}

	if launchMode != "prompt-only" {
		if req.IncludeDependencies {
			args = append(args, "--include-dependencies")
		}
		for _, arg := range promptArgs {
			args = append(args, "--prompt-arg", arg)
		}
		switch selectionMode {
		case "issues":
			issues := parsePortalIssueNumbers(req.Issues)
			if len(issues) == 0 {
				return nil, fmt.Errorf("no issues selected")
			}
			for _, issue := range issues {
				args = append(args, strconv.Itoa(issue))
			}
		case "label":
			if strings.TrimSpace(req.Label) == "" {
				return nil, fmt.Errorf("label selection requires a label")
			}
			args = append(args, "--label", strings.TrimSpace(req.Label))
		case "query":
			if strings.TrimSpace(req.Query) == "" {
				return nil, fmt.Errorf("query selection requires a query")
			}
			args = append(args, "--query", strings.TrimSpace(req.Query))
		case "next":
			next := 1
			if req.Next != nil {
				next = *req.Next
			}
			if next <= 0 {
				next = 1
			}
			args = append(args, "--next", strconv.Itoa(next))
		default:
			return nil, fmt.Errorf("unknown selection mode %q", selectionMode)
		}
	} else {
		for _, arg := range promptArgs {
			args = append(args, "--prompt-arg", arg)
		}
	}

	return args, nil
}

func promptArgsMap(args []string) map[string]string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func parsePortalPromptArgs(raw string) []string {
	lines := strings.Split(raw, "\n")
	args := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		args = append(args, line)
	}
	return args
}

func parsePortalIssueNumbers(raw string) []int {
	raw = strings.ReplaceAll(raw, ",", " ")
	parts := strings.Fields(raw)
	issues := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		issues = append(issues, n)
	}
	return issues
}

func readPortalTemplate(repoRoot, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read template file: %w", err)
	}
	return string(data), nil
}

func startPortalRun(ctx context.Context, repoRoot string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve sandman executable: %w", err)
	}
	_ = ctx
	cmd := exec.CommandContext(context.Background(), exe, args...)
	cmd.Dir = repoRoot
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sandman run: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
