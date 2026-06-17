package runid

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var runsDirRoot = ".sandman/runs"

var shortIDFunc = func() string {
	return fmt.Sprintf("%04x", time.Now().UnixNano()%0xFFFF)
}

var timeFunc = func() time.Time { return time.Now() }

type Kind int

const (
	KindIssue Kind = iota
	KindReview
	KindAutoSelect
	KindPromptOnly
)

func (k Kind) String() string {
	switch k {
	case KindIssue:
		return "issue"
	case KindReview:
		return "review"
	case KindAutoSelect:
		return "auto-select"
	case KindPromptOnly:
		return "prompt-only"
	default:
		return "unknown"
	}
}

func NewBatch() (ts, shortid string, err error) {
	ts = timeFunc().Format("20060102-150405")
	for attempt := 0; attempt < 16; attempt++ {
		shortid = shortIDFunc()
		entries, err := os.ReadDir(runsDirRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return ts, shortid, nil
			}
			return "", "", fmt.Errorf("read runs dir: %w", err)
		}
		collision := false
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ts+"-"+shortid) {
				collision = true
				break
			}
		}
		if !collision {
			return ts, shortid, nil
		}
	}
	return "", "", fmt.Errorf("failed to generate unique shortid after 16 attempts")
}

func NewBatchID(kind Kind, n int, firstSubject string, ts, shortid string) string {
	switch kind {
	case KindIssue:
		return fmt.Sprintf("%s-%s-%d-issues-first-%s", ts, shortid, n, firstSubject)
	case KindReview:
		return fmt.Sprintf("%s-%s-review-%s", ts, shortid, firstSubject)
	case KindAutoSelect:
		return fmt.Sprintf("%s-%s-auto-select-%d-candidates", ts, shortid, n)
	case KindPromptOnly:
		if firstSubject == "" {
			return fmt.Sprintf("%s-%s-prompt-only", ts, shortid)
		}
		return fmt.Sprintf("%s-%s-prompt-only-ID-%s", ts, shortid, firstSubject)
	default:
		return ""
	}
}

func NewRunID(kind Kind, subject string, ts, shortid string) string {
	return fmt.Sprintf("%s-%s-%s", ts, shortid, subject)
}

var userRunIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func IsValidUserRunID(s string) error {
	if s == "" {
		return fmt.Errorf("run id cannot be empty")
	}
	if len(s) > 64 {
		return fmt.Errorf("run id cannot exceed 64 characters")
	}
	if !userRunIDRe.MatchString(s) {
		return fmt.Errorf("run id must contain only alphanumerics, hyphens, and underscores")
	}
	return nil
}

func KindFromDirName(name string) (Kind, bool) {
	if strings.Contains(name, "-issues-first-") {
		return KindIssue, true
	}
	if strings.Contains(name, "-review-") {
		return KindReview, true
	}
	if strings.Contains(name, "-auto-select-") && strings.HasSuffix(name, "-candidates") {
		return KindAutoSelect, true
	}
	if strings.Contains(name, "-prompt-only") {
		return KindPromptOnly, true
	}
	return 0, false
}

func ParseBatchID(batchID string) (ts, shortid string, kind Kind, n int, subject string, err error) {
	parts := strings.Split(batchID, "-")
	if len(parts) < 4 {
		return "", "", 0, 0, "", fmt.Errorf("invalid batch ID format: %q", batchID)
	}
	ts = parts[0] + "-" + parts[1]
	shortid = parts[2]

	switch {
	case strings.HasPrefix(batchID, ts+"-"+shortid+"-prompt-only-ID-"):
		kind = KindPromptOnly
		subject = strings.TrimPrefix(batchID, ts+"-"+shortid+"-prompt-only-ID-")
	case batchID == ts+"-"+shortid+"-prompt-only":
		kind = KindPromptOnly
		subject = ""
	case strings.HasSuffix(batchID, "-candidates") && strings.Contains(batchID, "-auto-select-"):
		kind = KindAutoSelect
		prefix := ts + "-" + shortid + "-auto-select-"
		nStr := strings.TrimSuffix(strings.TrimPrefix(batchID, prefix), "-candidates")
		fmt.Sscanf(nStr, "%d", &n)
		subject = ""
	case strings.HasPrefix(batchID, ts+"-"+shortid+"-review-"):
		kind = KindReview
		subject = strings.TrimPrefix(batchID, ts+"-"+shortid+"-review-")
	case strings.HasPrefix(batchID, ts+"-"+shortid+"-issues-first-"):
		kind = KindIssue
		rest := strings.TrimPrefix(batchID, ts+"-"+shortid+"-")
		idx := strings.Index(rest, "-issues-first-")
		if idx > 0 {
			nStr := rest[:idx]
			fmt.Sscanf(nStr, "%d", &n)
			subject = strings.TrimPrefix(rest, nStr+"-issues-first-")
		}
	default:
		return "", "", 0, 0, "", fmt.Errorf("unrecognized batch ID format: %q", batchID)
	}
	return ts, shortid, kind, n, subject, nil
}

func ParseRunID(runID string) (ts, shortid string, kind Kind, subject string, err error) {
	parts := strings.Split(runID, "-")
	if len(parts) < 3 {
		return "", "", 0, "", fmt.Errorf("invalid run ID format: %q", runID)
	}
	ts = parts[0] + "-" + parts[1]
	shortid = parts[2]

	switch {
	case strings.HasPrefix(runID, ts+"-"+shortid+"-prompt-"):
		kind = KindPromptOnly
		subject = strings.TrimPrefix(runID, ts+"-"+shortid+"-prompt-")
	case strings.HasPrefix(runID, ts+"-"+shortid+"-auto-select-"):
		kind = KindAutoSelect
		subject = strings.TrimPrefix(runID, ts+"-"+shortid+"-auto-select-")
	case strings.HasPrefix(runID, ts+"-"+shortid+"-issue-"):
		if strings.Contains(runID, "-review-") {
			kind = KindReview
			rest := strings.TrimPrefix(runID, ts+"-"+shortid+"-issue-")
			idx := strings.Index(rest, "-review-")
			if idx > 0 {
				subject = rest
			}
		} else {
			kind = KindIssue
			subject = strings.TrimPrefix(runID, ts+"-"+shortid+"-issue-")
		}
	case strings.HasPrefix(runID, ts+"-"+shortid+"-review-"):
		kind = KindReview
		subject = strings.TrimPrefix(runID, ts+"-"+shortid+"-review-")
	default:
		return "", "", 0, "", fmt.Errorf("unrecognized run ID format: %q", runID)
	}
	return ts, shortid, kind, subject, nil
}
