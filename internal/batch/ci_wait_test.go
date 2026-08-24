package batch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestCIWaitEvidencePersistsDeadlinePerHead(t *testing.T) {
	workDir := t.TempDir()
	s := &runSession{}
	pr := &github.PR{Number: 17, HeadRefOid: "first-head"}

	first, err := s.ciWaitEvidence(workDir, pr, "first-head")
	if err != nil {
		t.Fatalf("first CI wait evidence: %v", err)
	}
	second, err := s.ciWaitEvidence(workDir, pr, "first-head")
	if err != nil {
		t.Fatalf("second CI wait evidence: %v", err)
	}
	firstWait := first["ci_wait"].(map[string]any)
	secondWait := second["ci_wait"].(map[string]any)
	if firstWait["deadline_unix_seconds"] != secondWait["deadline_unix_seconds"] {
		t.Fatalf("same-head deadline renewed: first=%v second=%v", firstWait, secondWait)
	}

	pr.HeadRefOid = "second-head"
	reset, err := s.ciWaitEvidence(workDir, pr, "second-head")
	if err != nil {
		t.Fatalf("reset CI wait evidence: %v", err)
	}
	resetWait := reset["ci_wait"].(map[string]any)
	if resetWait["head_sha"] != "second-head" {
		t.Fatalf("head SHA = %v, want second-head", resetWait["head_sha"])
	}
	path := filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json")
	registration, err := readCIWaitRegistration(path)
	if err != nil {
		t.Fatalf("read persisted CI wait: %v", err)
	}
	if registration.HeadSHA != "second-head" || registration.EffectiveTimeoutSecs != int64(ciWaitTimeout/time.Second) {
		t.Fatalf("registration = %#v", registration)
	}
}

func TestLifecycleDeadlineUsesCIWaitWhenNoReviewRequest(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Truncate(time.Second)
	got, gate, ok := lifecycleDeadline(map[string]any{
		"ci_wait": map[string]any{"deadline_unix_seconds": deadline.Unix()},
	})
	if !ok || gate != gateCIWaitTimeout || !got.Equal(deadline) {
		t.Fatalf("CI deadline = (%v, %q, %t), want (%v, %q, true)", got, gate, ok, deadline, gateCIWaitTimeout)
	}
}
