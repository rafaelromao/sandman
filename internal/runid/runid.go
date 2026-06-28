package runid

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var batchesDirRoot = ".sandman/batches"

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
	return NewBatchIn(batchesDirRoot)
}

func NewBatchIn(baseRunsDir string) (ts, shortid string, err error) {
	ts = timeFunc().Format("060102150405")
	for attempt := 0; attempt < 16; attempt++ {
		shortid = shortIDFunc()
		entries, err := os.ReadDir(baseRunsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return ts, shortid, nil
			}
			return "", "", fmt.Errorf("read batches dir: %w", err)
		}
		collision := false
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), shortid+"-"+ts) {
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
		return fmt.Sprintf("%s-%s-%s+%d", shortid, ts, firstSubject, n)
	case KindReview:
		return fmt.Sprintf("%s-%s-PR%s", shortid, ts, firstSubject)
	case KindAutoSelect:
		return fmt.Sprintf("%s-%s-auto-%d", shortid, ts, n)
	case KindPromptOnly:
		if firstSubject == "" {
			return fmt.Sprintf("%s-%s", shortid, ts)
		}
		return fmt.Sprintf("%s-%s-%s", shortid, ts, firstSubject)
	default:
		return ""
	}
}

func NewRunID(kind Kind, subject string, ts, shortid string) string {
	if subject == "" {
		return fmt.Sprintf("%s-%s", shortid, ts)
	}
	return fmt.Sprintf("%s-%s-%s", shortid, ts, subject)
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

var newFormatRe = regexp.MustCompile(`^[0-9a-f]{4}-\d{12}`)

func KindFromDirName(name string) (Kind, bool) {
	// New format: {sid}-{ts}-{rest}
	if strings.Contains(name, "-auto-") {
		return KindAutoSelect, true
	}
	if strings.Contains(name, "-PR") {
		return KindReview, true
	}
	// New format issue batch: {sid}-{ts}-{n}+{count} (contains "+")
	if strings.Contains(name, "+") {
		return KindIssue, true
	}
	// Old format: {ts}-{sid}-{rest}
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
	// New format PromptOnly: {sid}-{ts} or {sid}-{ts}-{userid}
	// Only when name matches new format prefix and no other marker found.
	if newFormatRe.MatchString(name) {
		return KindPromptOnly, true
	}
	return 0, false
}
