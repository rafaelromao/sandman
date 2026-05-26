package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

type portalCommandLaunchRequest struct {
	Preset      string `json:"preset"`
	Issue       int    `json:"issue,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	CleanMode   string `json:"cleanMode,omitempty"`
	Confirmed   bool   `json:"confirmed,omitempty"`
	ConfigMode  string `json:"configMode,omitempty"`
	ConfigKey   string `json:"configKey,omitempty"`
	ConfigValue string `json:"configValue,omitempty"`
	Command     string `json:"command,omitempty"`
}

func buildPortalCommandArgs(req portalCommandLaunchRequest) ([]string, error) {
	switch strings.TrimSpace(req.Preset) {
	case "continue":
		if req.Issue <= 0 {
			return nil, fmt.Errorf("continue preset requires an issue number")
		}
		if strings.TrimSpace(req.Prompt) == "" {
			return nil, fmt.Errorf("continue preset requires a prompt")
		}
		return []string{"continue", strconv.Itoa(req.Issue), strings.TrimSpace(req.Prompt)}, nil
	case "status":
		return []string{"status"}, nil
	case "history":
		return []string{"history"}, nil
	case "clean":
		if !req.Confirmed {
			return nil, fmt.Errorf("clean preset requires confirmation")
		}
		scope := strings.TrimSpace(req.CleanMode)
		if scope == "" {
			scope = "success"
		}
		switch scope {
		case "all":
			return []string{"clean", "--all"}, nil
		case "success":
			return []string{"clean", "--success"}, nil
		case "failed":
			return []string{"clean", "--failed"}, nil
		default:
			return nil, fmt.Errorf("unknown clean mode %q", req.CleanMode)
		}
	case "config":
		mode := strings.TrimSpace(req.ConfigMode)
		if mode == "" {
			mode = "get"
		}
		key := strings.TrimSpace(req.ConfigKey)
		if key == "" {
			return nil, fmt.Errorf("config preset requires a key")
		}
		switch mode {
		case "get":
			return []string{"config", "get", key}, nil
		case "set":
			if strings.TrimSpace(req.ConfigValue) == "" {
				return nil, fmt.Errorf("config set preset requires a value")
			}
			return []string{"config", "set", key, strings.TrimSpace(req.ConfigValue)}, nil
		default:
			return nil, fmt.Errorf("unknown config mode %q", req.ConfigMode)
		}
	default:
		return nil, fmt.Errorf("unknown preset %q", req.Preset)
	}
}
