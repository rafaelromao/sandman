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
