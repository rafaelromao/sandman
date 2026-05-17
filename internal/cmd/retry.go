package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// NewRetryCmd creates the retry command.
func NewRetryCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "retry [issue-number]",
		Short: "Retry the last agent run for an issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("no issue provided")
			}

			issueNum, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid issue number %q: %w", args[0], err)
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			var lastRun events.Event
			for _, e := range eventsList {
				if e.Type == "run.started" && e.Issue == issueNum {
					lastRun = e
				}
			}

			if lastRun.RunID == "" {
				return fmt.Errorf("no previous run found for issue #%d", issueNum)
			}

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			branch, _ := lastRun.Payload["branch"].(string)
			promptCfg, err := retryPromptConfig(cfg, lastRun)
			if err != nil {
				return err
			}
			if promptCfg.TemplateFlag != "" {
				if _, err := os.Stat(promptCfg.TemplateFlag); err != nil {
					if os.IsNotExist(err) {
						return fmt.Errorf("stored template path %q no longer exists", promptCfg.TemplateFlag)
					}
					return fmt.Errorf("check stored template path: %w", err)
				}
			}
			req := batch.Request{
				Issues:       []int{issueNum},
				Branches:     map[int]string{issueNum: branch},
				PromptConfig: promptCfg,
			}
			result, err := deps.BatchRunner.RunBatch(cmd.Context(), req)
			if result != nil {
				printSummary(cmd, result)
			}
			if err != nil {
				return fmt.Errorf("run batch: %w", err)
			}

			return nil
		},
	}
}

func retryPromptConfig(cfg *config.Config, run events.Event) (prompt.RenderConfig, error) {
	result := prompt.RenderConfig{ReviewCommand: effectiveReviewCommand(cfg)}
	if run.Payload == nil {
		return result, nil
	}

	if reviewCommand, ok := payloadString(run.Payload, "review_command"); ok {
		result.ReviewCommand = reviewCommand
		result.ReviewCommandSet = true
	}

	if promptArgs, ok, err := payloadPromptArgs(run.Payload["prompt_args"]); err != nil {
		return prompt.RenderConfig{}, fmt.Errorf("parse prompt args: %w", err)
	} else if ok {
		result.PromptArgs = promptArgs
	}

	sourceType, hasSourceType := payloadString(run.Payload, "prompt_source_type")
	if !hasSourceType || sourceType == "current" {
		return result, nil
	}

	sourceValue, ok := payloadString(run.Payload, "prompt_source_value")
	if !ok || sourceValue == "" {
		return prompt.RenderConfig{}, fmt.Errorf("missing prompt_source_value for %s source", sourceType)
	}

	switch sourceType {
	case "prompt":
		result.PromptFlag = sourceValue
	case "template":
		result.TemplateFlag = sourceValue
	default:
		return prompt.RenderConfig{}, fmt.Errorf("unknown prompt source type %q", sourceType)
	}

	return result, nil
}

func effectiveReviewCommand(cfg *config.Config) string {
	if cfg == nil {
		return config.DefaultReviewCommand
	}
	return cfg.EffectiveReviewCommand()
}

func payloadString(payload map[string]any, key string) (string, bool) {
	v, ok := payload[key]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok
}

func payloadPromptArgs(value any) (map[string]string, bool, error) {
	if value == nil {
		return nil, false, nil
	}
	args := map[string]string{}
	switch raw := value.(type) {
	case map[string]any:
		if len(raw) == 0 {
			return nil, false, nil
		}
		args = make(map[string]string, len(raw))
		for key, v := range raw {
			str, ok := v.(string)
			if !ok {
				return nil, false, fmt.Errorf("prompt arg %q has unexpected type %T", key, v)
			}
			args[key] = str
		}
	case map[string]string:
		if len(raw) == 0 {
			return nil, false, nil
		}
		args = make(map[string]string, len(raw))
		for key, v := range raw {
			args[key] = v
		}
	default:
		return nil, false, fmt.Errorf("unexpected type %T", value)
	}
	return args, true, nil
}
