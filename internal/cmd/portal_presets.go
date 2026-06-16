package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

type portalCommandLaunchRequest struct {
	Preset               string `json:"preset"`
	Issue                int    `json:"issue,omitempty"`
	Issues               []int  `json:"issues,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	CleanMode            string `json:"cleanMode,omitempty"`
	Confirmed            bool   `json:"confirmed,omitempty"`
	ConfigMode           string `json:"configMode,omitempty"`
	ConfigKey            string `json:"configKey,omitempty"`
	ConfigValue          string `json:"configValue,omitempty"`
	ArchiveMode          string `json:"archiveMode,omitempty"`
	ArchiveRunID         string `json:"archiveRunId,omitempty"`
	ArchiveOlderThanDays string `json:"archiveOlderThanDays,omitempty"`
}

func buildPortalCommandArgs(req portalCommandLaunchRequest) ([]string, error) {
	switch strings.TrimSpace(req.Preset) {
	case "continue":
		if len(req.Issues) == 0 {
			return nil, fmt.Errorf("continue preset requires issue numbers")
		}
		args := []string{"run", "--continue"}
		for _, issue := range req.Issues {
			args = append(args, strconv.Itoa(issue))
		}
		return args, nil
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
	case "archive":
		if !req.Confirmed {
			return nil, fmt.Errorf("archive preset requires confirmation")
		}
		return buildPortalArchiveArgs(req)
	default:
		return nil, fmt.Errorf("unknown preset %q", req.Preset)
	}
}

func buildPortalArchiveArgs(req portalCommandLaunchRequest) ([]string, error) {
	switch strings.TrimSpace(req.ArchiveMode) {
	case "run":
		id := strings.TrimSpace(req.ArchiveRunID)
		if id == "" {
			return nil, fmt.Errorf("archive run preset requires a run id")
		}
		return []string{"archive", "run", id}, nil
	case "older-than":
		raw := strings.TrimSpace(req.ArchiveOlderThanDays)
		if raw == "" {
			return nil, fmt.Errorf("archive older-than preset requires a day count")
		}
		days, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("archive older-than days %q is not a non-negative integer", raw)
		}
		if days < 0 {
			return nil, fmt.Errorf("archive older-than days %d is negative; must be non-negative", days)
		}
		return []string{"archive", "older-than", strconv.Itoa(days)}, nil
	case "stale":
		return []string{"archive", "stale"}, nil
	default:
		return nil, fmt.Errorf("unknown archive mode %q", req.ArchiveMode)
	}
}
