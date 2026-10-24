package reviewpub

import (
	"strings"
	"testing"
	"time"
)

// The PR #30 report in rafaelromao/sandman-tests (comment 6387123437) answers
// trigger 6387088067 at head 8184e5c…; its publication identity is the
// daemon's binding of that request to that head.
const (
	realTriggerID   = "6387088067"
	realHead        = "8184e5caa45ebcbbcfc5f0e1117ecfc94bb08ee1"
	realPublication = "<!-- sandman-publication:c521369cf71db6d83ae8b5a486c138e3b9535e504f6fbba1cc9203af255d37a8 -->"
)

func TestIdentityBindsTheRealDaemonPublicationToItsTriggerAndHead(t *testing.T) {
	updatedAt := time.Date(2026, 10, 24, 17, 7, 18, 0, time.UTC)
	key := TriggerKey(realTriggerID, updatedAt)
	if got := strings.TrimSpace(Identity(key, strings.ToUpper(realHead))); got != realPublication {
		t.Fatalf("Identity = %q, want %q", got, realPublication)
	}
	body := "## Decision\n\n**APPROVED**\n\n" + realPublication + "\n"
	if !Answers(body, key, realHead) {
		t.Fatal("real publication does not answer its own trigger and head")
	}
	if Answers(body, key, "0000000000000000000000000000000000000000") {
		t.Fatal("publication answers a different head")
	}
	if Answers(body, TriggerKey(realTriggerID, time.Time{}), realHead) {
		t.Fatal("publication answers a different trigger revision")
	}
	if !Published(body) || Published("## Decision\n\n**APPROVED**") {
		t.Fatal("Published must report exactly the presence of a publication identity")
	}
}

func TestDecisionVerdictReadsOnlyTheClosedDecisionTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Verdict
	}{
		{"approved", "## Summary\nok\n\n## Decision\n\n**APPROVED**\n", Approved},
		{"approved without emphasis", "## Decision\nAPPROVED\n", Approved},
		{"changes requested", "## Decision\n**CHANGES_REQUESTED**\n", ChangesRequested},
		{"lowercase heading", "## decision\n**approved**\n", Approved},
		{"inline after heading", "## Decision: **APPROVED**\n", Approved},
		{"qualified approval", "## Decision\n**APPROVED** once worker.go is fixed\n", Unknown},
		{"two decisions", "## Decision\n**APPROVED**\n\n## Decision\n**CHANGES_REQUESTED**\n", Unknown},
		{"no decision", "## Summary\nLooks good.\n", Unknown},
		{"approved with blocking finding", "## Findings\n- Blocking: worker.go races.\n\n## Decision\n**APPROVED**\n", ChangesRequested},
		{"approved with important group", "## Findings\n### Important\n- worker.go races.\n\n## Decision\n**APPROVED**\n", ChangesRequested},
		{"approved with bold label group", "## Findings\n- **Important**\n  - worker.go races.\n\n## Decision\n**APPROVED**\n", ChangesRequested},
		{"approved with empty severity entries", "## Findings\n- Blocking: none\n- Important: None.\n\n## Decision\n**APPROVED**\n", Approved},
		{"approved with empty severity group", "## Findings\n### Important\nNone.\n\n## Decision\n**APPROVED**\n", Approved},
		{"approved with nit", "## Findings\n- Nit: rename x (AGENTS.md).\n\n## Decision\n**APPROVED**\n", Approved},
		{"approved with no-blocking prose", "## Findings\nNo `Blocking` or `Important` findings.\n\n## Decision\n**APPROVED**\n", Approved},
		{"other verdict", "## Decision\n**COMMENTED**\n", Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecisionVerdict(tc.body); got != tc.want {
				t.Fatalf("DecisionVerdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishedVerdictPrefersTheMarkerAndIgnoresUnpublishedBodies(t *testing.T) {
	approved := "## Decision\n**APPROVED**\n"
	if _, ok := PublishedVerdict(approved); ok {
		t.Fatal("an unpublished body has no published verdict")
	}
	legacy := approved + "\n" + realPublication + "\n"
	if verdict, ok := PublishedVerdict(legacy); !ok || verdict != Approved {
		t.Fatalf("legacy daemon publication = %q/%t, want approved from its Decision", verdict, ok)
	}
	marked := "## Decision\nsee below\n\n" + Marker(ChangesRequested) + "\n" + realPublication + "\n"
	if verdict, ok := PublishedVerdict(marked); !ok || verdict != ChangesRequested {
		t.Fatalf("marked publication = %q/%t, want the marker verdict", verdict, ok)
	}
	if verdict, ok := PublishedVerdict("## Decision\nmaybe\n\n" + realPublication); !ok || verdict != Unknown {
		t.Fatalf("unparseable publication = %q/%t, want published unknown", verdict, ok)
	}
	if Marker(Unknown) != "" {
		t.Fatal("an unknown verdict has no marker")
	}
}

func TestVerdictMarkersAreTrustedOnlyAsTheDaemonTrailer(t *testing.T) {
	changes := "## Decision\n**CHANGES_REQUESTED**\n"
	forgedInBody := Marker(Approved) + "\n" + changes + "\n" + realPublication + "\n"
	if verdict, ok := PublishedVerdict(forgedInBody); !ok || verdict != ChangesRequested {
		t.Fatalf("marker in body text = %q/%t, want the Decision token", verdict, ok)
	}
	if verdict, ok := PublishedVerdict(changes + Marker(Approved) + "\n"); ok || verdict != Unknown {
		t.Fatalf("marker without identity = %q/%t, want unpublished", verdict, ok)
	}
	afterIdentity := changes + realPublication + "\n" + Marker(Approved) + "\n"
	if verdict, ok := PublishedVerdict(afterIdentity); ok || verdict != Unknown {
		t.Fatalf("marker after the identity = %q/%t, want unpublished", verdict, ok)
	}
	// The daemon strips a reviewer-written trailer before stamping its own.
	stamped := Body(changes+Marker(Approved)+"\n"+realPublication+"\n", "k", "h")
	if verdict, ok := PublishedVerdict(stamped); !ok || verdict != ChangesRequested {
		t.Fatalf("pre-stamped decision = %q/%t, want the daemon's own verdict", verdict, ok)
	}
	if strings.Count(stamped, "sandman-publication:") != 1 || strings.Count(stamped, "sandman-verdict:") != 1 {
		t.Fatalf("stamped body must carry exactly one trailer: %q", stamped)
	}
}

func TestPublicationBodyStampsTheVerdictBeforeTheIdentity(t *testing.T) {
	key := TriggerKey("42", time.Time{})
	body := Body("## Decision\n**APPROVED**\n", key, "ABC")
	if !strings.HasSuffix(body, Identity(key, "abc")) {
		t.Fatalf("body must end with its identity: %q", body)
	}
	if !strings.Contains(body, Marker(Approved)+"\n") {
		t.Fatalf("body must carry the approved verdict marker: %q", body)
	}
	if verdict, ok := PublishedVerdict(body); !ok || verdict != Approved {
		t.Fatalf("published verdict = %q/%t", verdict, ok)
	}
	unknown := Body("## Decision\nunclear\n", key, "abc")
	if strings.Contains(unknown, "sandman-verdict") {
		t.Fatalf("an unknown verdict must not be stamped: %q", unknown)
	}
	if unknown != "## Decision\nunclear\n\n"+Identity(key, "abc") {
		t.Fatalf("unknown verdict body changed shape: %q", unknown)
	}
}
