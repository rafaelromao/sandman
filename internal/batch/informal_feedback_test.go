package batch

import (
	"encoding/json"
	"strings"
	"testing"
)

func informalClassificationForTest(t *testing.T, mutate func(map[string]any)) *reviewClassification {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{
	  "protocol": "review-classification/v1",
	  "request": {
	    "repository": "owner/repo",
	    "pull_request": 17,
	    "head_sha": "current-sha",
	    "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
	    "trigger_prefix": "/sandman review",
	    "trigger_created_at": "1970-01-01T00:16:40Z",
	    "deadline_at": "unix:2800",
	    "deadline_unix_seconds": 2800
	  },
	  "observed_head_sha": "current-sha",
	  "request_state": "active",
	  "decision": "responded",
	  "window": {
	    "start": "1970-01-01T00:16:40Z",
	    "end": null,
	    "deadline_at": "unix:2800",
	    "deadline_unix_seconds": 2800,
	    "next_trigger": null
	  },
	  "response_counts": {"top_level": 1, "formal_reviews": 0, "inline_comments": 0},
	  "sources": {
	    "top_level": [
	      {
	        "id": "issuecomment-2001",
	        "source": "top_level",
	        "response_timestamp": "1970-01-01T00:20:00Z",
	        "head_status": "current",
	        "url": "https://github.com/owner/repo/pull/17#issuecomment-2001",
	        "body": "Please fix the race in internal/socketpath/socketpath.go: the listener close is not synchronized."
	      }
	    ],
	    "formal_reviews": [],
	    "inline_comments": []
	  },
	  "formal": {
	    "decision": "none",
	    "approval_evidence": [],
	    "ambiguous_approval_evidence": [],
	    "requested_changes": []
	  },
	  "boundary_evidence": {
	    "request": {
	      "repository": "owner/repo",
	      "pull_request": 17,
	      "head_sha": "current-sha",
	      "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
	      "trigger_prefix": "/sandman review",
	      "trigger_created_at": "1970-01-01T00:16:40Z",
	      "deadline_at": "unix:2800",
	      "deadline_unix_seconds": 2800
	    },
	    "sources": {
	      "top_level": [
	        {
	          "id": "issuecomment-2001",
	          "source": "top_level",
	          "response_timestamp": "1970-01-01T00:20:00Z",
	          "head_status": "current",
	          "url": "https://github.com/owner/repo/pull/17#issuecomment-2001",
	          "body": "concrete"
	        }
	      ],
	      "formal_reviews": [],
	      "inline_comments": []
	    }
	  }
	}`), &raw); err != nil {
		t.Fatalf("seed classification: %v", err)
	}
	classification := &reviewClassification{Raw: raw}
	classification.RequestState = "active"
	classification.Decision = "responded"
	classification.FormalDecision = "none"
	classification.ResponseCounts = reviewResponseCounts{TopLevel: 1}
	classification.WindowEnd = ""
	if mutate != nil {
		mutate(raw)
	}
	return classification
}

func informalRequestForTest() reviewRequestEnvelope {
	return reviewRequestEnvelope{
		Protocol:            "review-wait/v1",
		Repository:          "owner/repo",
		PullRequest:         17,
		HeadSHA:             "current-sha",
		TriggerID:           "https://github.com/owner/repo/pull/17#issuecomment-1001",
		TriggerPrefix:       "/sandman review",
		TriggerCreatedAt:    "1970-01-01T00:16:40Z",
		ConfirmedAt:         "1970-01-01T00:16:40Z",
		StartedAt:           "1970-01-01T00:16:40Z",
		DeadlineAt:          "unix:2800",
		StartedUnixSeconds:  1000,
		DeadlineUnixSeconds: 2800,
		EffectiveTimeout:    1800,
		PollPlan:            []int{120, 60, 60, 30},
	}
}

func TestInformalFeedbackConcrete(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"backtick code span", "Please rename `doThing` to `doOtherThing`.", true},
		{"file path", "The socket cleanup in internal/socketpath/socketpath.go is wrong.", true},
		{"file path with line", "Look at internal/batch/orchestrator.go:3142 and fix the await loop.", true},
		{"line number", "Please fix the issue on line 42.", true},
		{"diff marker", "- keep := true\n+ keep := false", true},
		{"single dash bullet is not a hunk", "- please check the docs", false},
		{"single plus bullet is not a hunk", "+ nice try", false},
		{"function call span", "`applyFixes()` never waits for the reviewer.", true},
		{"blank body", "   \n\t ", false},
		{"emoji only", "😃🎉", false},
		{"pure approval", "lgtm", false},
		{"short approval phrase", "looks good to me, thanks!", false},
		{"approval with symbol", "+1 👍", false},
		{"generic praise", "nice work!", false},
		{"generic request", "please improve this", false},
		{"ambiguous question", "can you take a look?", false},
		{"vague thanks", "thanks for the quick turnaround", false},
		{"no code anchor", "Please switch the bind mount to a shared mount.", false},
		{"praise with code anchor (bounded false positive)", "nice work on `loader.go`, thanks!", true},
		{"mention with code anchor (bounded false positive)", "please take a look at internal/batch/orchestrator.go when you have a chance", true},
		{"error message anchor (bounded false positive)", "getting `permission denied` on install", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := informalFeedbackConcrete(tc.body); got != tc.want {
				t.Fatalf("informalFeedbackConcrete(%q) = %t, want %t", tc.body, got, tc.want)
			}
		})
	}
}

func TestInformalFeedbackEvidenceFor(t *testing.T) {
	assertNone := func(t *testing.T, classification *reviewClassification) {
		t.Helper()
		evidence := classification.informalFeedbackEvidenceFor(informalRequestForTest(), classification.WindowEnd)
		if len(evidence) != 0 {
			t.Fatalf("informal evidence = %+v, want none", evidence)
		}
	}

	t.Run("concrete top-level yields full evidence record", func(t *testing.T) {
		classification := informalClassificationForTest(t, nil)
		evidence := classification.informalFeedbackEvidenceFor(informalRequestForTest(), classification.WindowEnd)
		if len(evidence) != 1 {
			t.Fatalf("informal evidence = %+v, want one record", evidence)
		}
		got := evidence[0]
		for key, want := range map[string]string{
			"Source":            "top_level",
			"ID":                "issuecomment-2001",
			"ResponseTimestamp": "1970-01-01T00:20:00Z",
			"HeadStatus":        "current",
			"Locator":           "https://github.com/owner/repo/pull/17#issuecomment-2001",
		} {
			switch key {
			case "Source":
				if got.Source != want {
					t.Fatalf("evidence source = %q, want %q", got.Source, want)
				}
			case "ID":
				if got.ID != want {
					t.Fatalf("evidence id = %q, want %q", got.ID, want)
				}
			case "ResponseTimestamp":
				if got.ResponseTimestamp != want {
					t.Fatalf("evidence response_timestamp = %q, want %q", got.ResponseTimestamp, want)
				}
			case "HeadStatus":
				if got.HeadStatus != want {
					t.Fatalf("evidence head_status = %q, want %q", got.HeadStatus, want)
				}
			case "Locator":
				if got.Locator != want {
					t.Fatalf("evidence locator = %q, want %q", got.Locator, want)
				}
			}
		}
		if !strings.Contains(got.Body, "socketpath/socketpath.go") {
			t.Fatalf("evidence body = %q, want the concrete retained body", got.Body)
		}
	})

	t.Run("concrete current inline with path and line yields file locator", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			sources := raw["sources"].(map[string]any)
			sources["top_level"] = []any{}
			sources["inline_comments"] = []any{map[string]any{
				"id": "discussion_r1", "source": "inline_comment",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "current",
				"commit_id": "current-sha", "path": "internal/batch/orchestrator.go", "line": 3142.0,
				"body": "Please hoist this `emitAwait` call out of the loop.",
			}}
			raw["response_counts"].(map[string]any)["top_level"] = 0
			raw["response_counts"].(map[string]any)["inline_comments"] = 1
		})
		evidence := classification.informalFeedbackEvidenceFor(informalRequestForTest(), classification.WindowEnd)
		if len(evidence) != 1 {
			t.Fatalf("informal evidence = %+v, want one inline record", evidence)
		}
		got := evidence[0]
		if got.Source != "inline_comments" || got.Locator != "internal/batch/orchestrator.go:3142" {
			t.Fatalf("inline evidence = %+v, want source inline_comments and file locator", got)
		}
	})

	t.Run("inline with nullable line falls back to original line then url", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			sources := raw["sources"].(map[string]any)
			sources["top_level"] = []any{}
			sources["inline_comments"] = []any{map[string]any{
				"id": "discussion_r2", "source": "inline_comment",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "current",
				"commit_id": "current-sha", "path": "internal/batch/row_spec.go",
				"line": nil, "original_line": 88,
				"body": "Update the `RowSpec` comment to match the new field.",
			}}
			raw["response_counts"].(map[string]any)["top_level"] = 0
			raw["response_counts"].(map[string]any)["inline_comments"] = 1
		})
		evidence := classification.informalFeedbackEvidenceFor(informalRequestForTest(), classification.WindowEnd)
		if len(evidence) != 1 {
			t.Fatalf("informal evidence = %+v, want one inline record", evidence)
		}
		if got := evidence[0].Locator; got != "internal/batch/row_spec.go:88" {
			t.Fatalf("inline locator = %q, want path:original_line", got)
		}
	})

	t.Run("boilerplate top-level yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["body"] = "looks good to me, thanks!"
		})
		assertNone(t, classification)
	})

	t.Run("generic top-level yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["body"] = "please improve this"
		})
		assertNone(t, classification)
	})

	t.Run("ambiguous top-level yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["body"] = "can you take a look?"
		})
		assertNone(t, classification)
	})

	t.Run("stale inline yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			sources := raw["sources"].(map[string]any)
			sources["top_level"] = []any{}
			sources["inline_comments"] = []any{map[string]any{
				"id": "discussion_r3", "source": "inline_comment",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "stale",
				"commit_id": "old-sha", "path": "a.go", "line": 1,
				"body": "Please fix this `emitAwait` call.",
			}}
			raw["response_counts"].(map[string]any)["top_level"] = 0
			raw["response_counts"].(map[string]any)["inline_comments"] = 1
		})
		assertNone(t, classification)
	})

	t.Run("unknown inline yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			sources := raw["sources"].(map[string]any)
			sources["top_level"] = []any{}
			sources["inline_comments"] = []any{map[string]any{
				"id": "discussion_r4", "source": "inline_comment",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "unknown",
				"path": "a.go", "line": 1,
				"body": "Please fix this `emitAwait` call.",
			}}
			raw["response_counts"].(map[string]any)["top_level"] = 0
			raw["response_counts"].(map[string]any)["inline_comments"] = 1
		})
		assertNone(t, classification)
	})

	t.Run("superseded request yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["request_state"] = "superseded"
			raw["decision"] = "pending"
		})
		classification.RequestState = "superseded"
		assertNone(t, classification)
	})

	t.Run("out-of-window timestamp yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, nil)
		request := informalRequestForTest()
		request.TriggerCreatedAt = "1970-01-01T00:46:40Z" // unix 2800 == deadline, so the record at 00:20 is before the deadline but before the start
		if evidence := classification.informalFeedbackEvidenceFor(request, classification.WindowEnd); len(evidence) != 0 {
			t.Fatalf("informal evidence = %+v, want none for out-of-window records", evidence)
		}
	})

	t.Run("next-trigger boundary truncates the window for an active request", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			// Defensive window-end handling: the boundary is passed in
			// explicitly, so an active request whose classification carries
			// a window end (a later trigger) must drop records after it.
			raw["window"].(map[string]any)["end"] = "1970-01-01T00:25:00Z"
		})
		classification.WindowEnd = "1970-01-01T00:25:00Z" // unix 1500: record at 00:20 (1200) is inside, a hypothetical later record is not
		request := informalRequestForTest()
		request.TriggerCreatedAt = "1970-01-01T00:10:00Z"
		request.DeadlineUnixSeconds = 3600 // unix 4000: the end boundary, not the deadline, truncates the window
		evidence := classification.informalFeedbackEvidenceFor(request, classification.WindowEnd)
		if len(evidence) != 1 {
			t.Fatalf("informal evidence = %+v, want the in-boundary record", evidence)
		}
		raw := informalClassificationForTest(t, func(informal map[string]any) {
			informal["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["response_timestamp"] = "1970-01-01T00:30:00Z"
		})
		if evidence := raw.informalFeedbackEvidenceFor(request, "1970-01-01T00:25:00Z"); len(evidence) != 0 {
			t.Fatalf("informal evidence = %+v, want none for records after the window end", evidence)
		}
	})

	t.Run("trigger-prefixed body yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["body"] = "/sandman review please check this"
		})
		assertNone(t, classification)
	})

	t.Run("empty body yields no evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["sources"].(map[string]any)["top_level"].([]any)[0].(map[string]any)["body"] = "   "
		})
		assertNone(t, classification)
	})

	t.Run("formal requested changes take precedence over informal evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["decision"] = "changes_requested"
			raw["formal"].(map[string]any)["decision"] = "changes_requested"
			raw["formal"].(map[string]any)["requested_changes"] = []any{map[string]any{
				"id": "review-3001", "source": "formal_review", "state": "CHANGES_REQUESTED",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "current", "commit_id": "current-sha",
			}}
			raw["sources"].(map[string]any)["formal_reviews"] = []any{map[string]any{
				"id": "review-3001", "source": "formal_review", "state": "CHANGES_REQUESTED",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "current", "commit_id": "current-sha",
			}}
			raw["response_counts"].(map[string]any)["formal_reviews"] = 1
			raw["boundary_evidence"].(map[string]any)["sources"].(map[string]any)["formal_reviews"] = []any{map[string]any{
				"id": "review-3001", "source": "formal_review", "state": "CHANGES_REQUESTED",
				"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "current", "commit_id": "current-sha",
			}}
		})
		classification.Decision = "changes_requested"
		classification.FormalDecision = "changes_requested"
		assertNone(t, classification)
	})

	t.Run("formal approval yields no informal evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["decision"] = "approved"
			raw["formal"].(map[string]any)["decision"] = "approved"
		})
		classification.Decision = "approved"
		classification.FormalDecision = "approved"
		assertNone(t, classification)
	})

	t.Run("ambiguous formal approval yields no informal evidence", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["decision"] = "pending"
			raw["formal"].(map[string]any)["decision"] = "ambiguous"
		})
		classification.Decision = "pending"
		classification.FormalDecision = "ambiguous"
		assertNone(t, classification)
	})

	t.Run("pending decision yields no evidence even with sources", func(t *testing.T) {
		classification := informalClassificationForTest(t, func(raw map[string]any) {
			raw["decision"] = "pending"
		})
		classification.Decision = "pending"
		assertNone(t, classification)
	})
}
