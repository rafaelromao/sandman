package batch

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

const ciRerunActionKey = "ci_rerun_action"

// ciRerunActionEvidence is the typed, agent-invoked helper result that proves
// the current workflow is a prerequisite of a diagnosed recovery. It is
// evidence for lifecycle selection, not a lifecycle status record.
type ciRerunActionEvidence struct {
	Protocol        string `json:"protocol"`
	State           string `json:"state"`
	Reason          string `json:"reason"`
	Repository      string `json:"repository"`
	PullRequest     int    `json:"pull_request"`
	SourceHeadSHA   string `json:"source_head_sha"`
	TestedSHA       string `json:"tested_sha"`
	Diagnosis       string `json:"diagnosis"`
	RunID           string `json:"run_id"`
	JobID           string `json:"job_id"`
	JobName         string `json:"job_name"`
	ObservedAttempt int    `json:"observed_attempt"`
}

// ciRerunActionReporter is implemented only by strategies that can capture a
// completed helper call from the agent CLI's structured tool stream. Ordinary
// rendered output is deliberately not an authorization channel.
type ciRerunActionReporter interface {
	CIRerunAction() *ciRerunActionEvidence
}

func ciRerunActionFromParsers(parsers ...outputParser) *ciRerunActionEvidence {
	for _, parser := range parsers {
		reporter, ok := parser.(ciRerunActionReporter)
		if !ok {
			continue
		}
		if evidence := reporter.CIRerunAction(); evidence != nil {
			return evidence
		}
	}
	return nil
}

const installedCIRerunHelper = "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh"

func ciRerunHelperToolEvidence(command, output string) *ciRerunActionEvidence {
	words, ok := ciRerunHelperCommandWords(command)
	if !ok {
		return nil
	}
	var evidence *ciRerunActionEvidence
	for _, line := range strings.Split(output, "\n") {
		var candidate ciRerunActionEvidence
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &candidate) == nil && candidate.Protocol == "ci-rerun/v1" {
			evidence = &candidate
		}
	}
	if evidence == nil {
		return nil
	}
	arguments := []string{
		"--repository", evidence.Repository,
		"--pull-request", strconv.Itoa(evidence.PullRequest),
		"--head-sha", evidence.SourceHeadSHA,
		"--tested-sha", evidence.TestedSHA,
		"--run-id", evidence.RunID,
		"--job-id", evidence.JobID,
		"--diagnosis", evidence.Diagnosis,
	}
	for index, argument := range arguments {
		if words[index+2] != argument {
			return nil
		}
	}
	return evidence
}

func ciRerunHelperCommandWords(command string) ([]string, bool) {
	words, ok := shellCommandWords(command)
	if !ok || len(words) != 16 || words[0] != "sh" || words[1] != installedCIRerunHelper {
		return nil, false
	}
	return words, true
}

// shellCommandWords accepts the helper's documented single-command form while
// rejecting shell composition and command substitution. This binds the
// structured tool result to one exact helper invocation, not a command that
// merely prints a protocol-shaped receipt.
func shellCommandWords(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for index, char := range command {
		if escaped {
			escaped = false
			if char == '\n' {
				continue
			}
			word.WriteRune(char)
			started = true
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				word.WriteRune(char)
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			started = true
		case ' ', '\t', '\r':
			flush()
		case '\n', ';', '|', '&', '<', '>', '`':
			return nil, false
		case '$':
			if index+1 < len(command) && command[index+1] == '(' {
				return nil, false
			}
			word.WriteRune(char)
			started = true
		default:
			word.WriteRune(char)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, false
	}
	flush()
	return words, true
}

func (e ciRerunActionEvidence) matchesPrerequisite(prerequisite map[string]any) bool {
	if prerequisite == nil {
		return false
	}
	prNumber, ok := lifecycleDeadlineSeconds(prerequisite["pull_request"])
	if !ok || prNumber != int64(e.PullRequest) || e.PullRequest <= 0 {
		return false
	}
	repository, _ := prerequisite["repository"].(string)
	head, _ := prerequisite["head_sha"].(string)
	workflowRunID, _ := prerequisite["workflow_run_id"].(string)
	if !e.matchesPullRequest(e.PullRequest, repository, head) || e.RunID != strings.TrimSpace(workflowRunID) {
		return false
	}
	failedChecks, ok := ciRerunFailedChecks(prerequisite["failed_checks"])
	if !ok {
		return false
	}
	workflowCheckNames, ok := ciRerunWorkflowCheckNames(prerequisite["workflow_check_names"])
	if !ok {
		return false
	}
	return ciRerunFailuresCoveredBySelectedJob(failedChecks, workflowCheckNames, e.JobName)
}

func ciRerunFailedChecks(value any) ([]github.CIFailedCheck, bool) {
	var checks []github.CIFailedCheck
	switch values := value.(type) {
	case []github.CIFailedCheck:
		checks = append(checks, values...)
	case []any:
		if len(values) == 0 {
			return nil, false
		}
		checks = make([]github.CIFailedCheck, 0, len(values))
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			name, nameOK := item["name"].(string)
			detailsURL, urlOK := item["details_url"].(string)
			if !nameOK || !urlOK {
				return nil, false
			}
			checks = append(checks, github.CIFailedCheck{Name: name, DetailsURL: detailsURL})
		}
	default:
		return nil, false
	}
	if len(checks) == 0 {
		return nil, false
	}
	identities := make(map[string]struct{}, len(checks))
	for index := range checks {
		checks[index].Name = strings.TrimSpace(checks[index].Name)
		checks[index].DetailsURL = strings.TrimSpace(checks[index].DetailsURL)
		if checks[index].Name == "" || checks[index].DetailsURL == "" {
			return nil, false
		}
		if _, duplicate := identities[checks[index].DetailsURL]; duplicate {
			return nil, false
		}
		identities[checks[index].DetailsURL] = struct{}{}
	}
	return checks, true
}

func ciRerunWorkflowCheckNames(value any) ([]string, bool) {
	var names []string
	switch values := value.(type) {
	case []string:
		names = append(names, values...)
	case []any:
		if len(values) == 0 {
			return nil, false
		}
		for _, value := range values {
			name, ok := value.(string)
			if !ok {
				return nil, false
			}
			names = append(names, name)
		}
	default:
		return nil, false
	}
	if len(names) == 0 {
		return nil, false
	}
	for index := range names {
		names[index] = strings.TrimSpace(names[index])
		if names[index] == "" {
			return nil, false
		}
	}
	return names, true
}

func ciRerunFailuresCoveredBySelectedJob(failedChecks []github.CIFailedCheck, workflowCheckNames []string, selectedJobName string) bool {
	selectedJobName = strings.TrimSpace(selectedJobName)
	if selectedJobName == "" || len(failedChecks) == 0 || len(workflowCheckNames) == 0 {
		return false
	}
	selectedWorkflowChecks := 0
	for _, name := range workflowCheckNames {
		if strings.EqualFold(strings.TrimSpace(name), selectedJobName) {
			selectedWorkflowChecks++
		}
	}
	if selectedWorkflowChecks != 1 {
		return false
	}
	selectedFailures := 0
	for _, check := range failedChecks {
		name := strings.TrimSpace(check.Name)
		if name == "" || strings.TrimSpace(check.DetailsURL) == "" {
			return false
		}
		if strings.EqualFold(name, selectedJobName) {
			selectedFailures++
			continue
		}
		if strings.EqualFold(name, "quality-gate") {
			continue
		}
		return false
	}
	return selectedFailures == 1
}

func (e ciRerunActionEvidence) matchesPullRequest(pullRequest int, repository, head string) bool {
	waiting := e.State == "waiting" &&
		(e.Reason == "workflow-still-running" || e.Reason == "recovery-already-active") && e.ObservedAttempt >= 1
	confirmedRerun := e.State == "confirmed" &&
		e.Reason == "selected-job-and-quality-gate-rerun-observed" && e.ObservedAttempt >= 2
	if e.Protocol != "ci-rerun/v1" || (!waiting && !confirmedRerun) ||
		(e.Diagnosis != "transient-provider-timeout" && e.Diagnosis != "transient-network-timeout") ||
		strings.TrimSpace(e.TestedSHA) == "" {
		return false
	}
	runID, err := strconv.ParseUint(e.RunID, 10, 64)
	if err != nil || runID == 0 {
		return false
	}
	if jobID, err := strconv.ParseUint(e.JobID, 10, 64); err != nil || jobID == 0 {
		return false
	}
	if pullRequest <= 0 || e.PullRequest != pullRequest ||
		strings.TrimSpace(repository) == "" || strings.TrimSpace(head) == "" || strings.TrimSpace(e.JobName) == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(e.Repository), strings.TrimSpace(repository)) &&
		strings.EqualFold(strings.TrimSpace(e.SourceHeadSHA), strings.TrimSpace(head))
}

func (e ciRerunActionEvidence) payload() map[string]any {
	data, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	return payload
}

func ciRerunActionFromPayload(value any) (*ciRerunActionEvidence, bool) {
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	var evidence ciRerunActionEvidence
	if json.Unmarshal(data, &evidence) != nil || evidence.Protocol != "ci-rerun/v1" {
		return nil, false
	}
	return &evidence, true
}
