package reviewpub

import (
	"strings"
	"testing"
)

const graphqlBlocked = "FIXTURE_GRAPHQL=blocked"

// A managed sandbox without GraphQL still renders the daemon's review prompt:
// the same bytes the GraphQL path renders for the same pull request, with only
// a stderr notice to show the degradation.
func TestBlindPromptRendersIdenticallyWithoutGraphQL(t *testing.T) {
	for name, body := range map[string]string{
		"linked work item from the pull request body": "Closes #42",
		"empty pull request body":                     "",
	} {
		t.Run(name, func(t *testing.T) {
			viaGraphQL := runPromptHelper(t, body)
			if viaGraphQL.err != nil {
				t.Fatalf("GraphQL render failed: %v\n%s", viaGraphQL.err, viaGraphQL.stderr)
			}
			viaREST := runPromptHelper(t, body, graphqlBlocked)
			if viaREST.err != nil {
				t.Fatalf("REST render failed: %v\n%s", viaREST.err, viaREST.stderr)
			}
			if viaREST.stdout != viaGraphQL.stdout {
				t.Fatalf("REST prompt differs from the GraphQL prompt\nREST:\n%s\nGraphQL:\n%s", viaREST.stdout, viaGraphQL.stdout)
			}
			for _, want := range []string{"Checkout commit: `abc`", "Comparison base: `origin/main`", "Container compatibility"} {
				if !strings.Contains(viaREST.stdout, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			if body != "" && !strings.Contains(viaREST.stdout, "Acceptance criterion from issue 42") {
				t.Error("REST prompt lacks the linked work item's acceptance criteria")
			}
			if viaGraphQL.stderr != "" {
				t.Errorf("GraphQL mode must stay silent on stderr: %q", viaGraphQL.stderr)
			}
			if !strings.Contains(viaREST.stderr, "REST") {
				t.Errorf("REST mode must say so on stderr: %q", viaREST.stderr)
			}
		})
	}
}

// Degradation is selected by capability, never by guessing: a lookup that fails
// while GraphQL itself works stays a lookup failure and never reaches REST,
// and a head that moved is refused over REST as well.
func TestBlindPromptRefusalsStayRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		env    []string
		reason string
		rest   bool
	}{
		"lookup fails but GraphQL works": {[]string{graphqlBlocked, "FIXTURE_PROBE_OK=1"}, "REVIEW_PROMPT_LOOKUP_FAILED", false},
		"REST is unreachable too":        {[]string{graphqlBlocked, "FIXTURE_REST=down"}, "REVIEW_PROMPT_LOOKUP_FAILED", true},
		"stale head over REST":           {[]string{graphqlBlocked, "FIXTURE_STATE=stale"}, "REVIEW_PROMPT_STALE_HEAD", true},
		"closed pull request over REST":  {[]string{graphqlBlocked, "FIXTURE_STATE=closed"}, "REVIEW_PROMPT_STALE_HEAD", true},
	} {
		t.Run(name, func(t *testing.T) {
			run := runPromptHelper(t, "Closes #42", tc.env...)
			if run.err == nil || run.stdout != "" {
				t.Fatalf("helper must fail closed without a prompt: err=%v stdout=%q", run.err, run.stdout)
			}
			if !strings.Contains(run.stderr, tc.reason) {
				t.Fatalf("stderr = %q, want %s", run.stderr, tc.reason)
			}
			if got := run.called("api repos/"); got != tc.rest {
				t.Fatalf("REST fallback attempted = %t, want %t (calls: %v)", got, tc.rest, run.calls)
			}
		})
	}
}

// With GraphQL blocked the publication helper reads, posts once and confirms
// using REST only; stdout stays the unchanged envelope.
func TestBlindPublicationUsesRESTWhenGraphQLIsBlocked(t *testing.T) {
	run := runPublishHelper(t, nil, graphqlBlocked)
	if run.err != nil {
		t.Fatalf("REST publication failed: %v\nstderr=%s", run.err, run.stderr)
	}
	if run.mutations != 1 {
		t.Fatalf("REST round must post exactly once, posted %d times", run.mutations)
	}
	envelope := run.envelope(t)
	if envelope["state"] != "confirmed" || envelope["verdict"] != "approved" || envelope["head_sha"] != "abc" ||
		envelope["comment_url"] != "https://example.test/comment/1" {
		t.Fatalf("unexpected envelope: %v", envelope)
	}
	if run.called("pr comment") {
		t.Fatalf("a GraphQL-backed post was attempted after the probe failed: %v", run.calls)
	}
	if !run.called("api -X POST repos/owner/repo/issues/17/comments") {
		t.Fatalf("the round did not post through REST: %v", run.calls)
	}
	if !strings.Contains(run.stderr, "REST") {
		t.Errorf("REST mode must say so on stderr: %q", run.stderr)
	}
}

func TestBlindPublicationRESTRefusalsNeverMutate(t *testing.T) {
	for name, tc := range map[string]struct {
		env       []string
		reason    string
		post      bool
		mutations int
	}{
		"stale head":                        {[]string{graphqlBlocked, "FIXTURE_STATE=stale"}, "stale-or-closed", false, 0},
		"closed pull request":               {[]string{graphqlBlocked, "FIXTURE_STATE=closed"}, "stale-or-closed", false, 0},
		"REST unreachable":                  {[]string{graphqlBlocked, "FIXTURE_REST=down"}, "lookup-failed", false, 0},
		"lookup fails but GraphQL works":    {[]string{graphqlBlocked, "FIXTURE_PROBE_OK=1"}, "lookup-failed", false, 0},
		"post fails is uncertain":           {[]string{graphqlBlocked, "FIXTURE_POST_FAIL=1"}, "publication-uncertain", true, 0},
		"duplicate read-back candidates":    {[]string{graphqlBlocked, "FIXTURE_DUPLICATE_COMMENT=1"}, "confirmation-uncertain", true, 1},
		"duplicate candidates over GraphQL": {[]string{"FIXTURE_DUPLICATE_COMMENT=1"}, "confirmation-uncertain", false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			run := runPublishHelper(t, nil, tc.env...)
			if run.err == nil {
				t.Fatalf("helper must refuse: %s", run.stdout)
			}
			if refusal := run.refusal(t); refusal["reason"] != tc.reason {
				t.Fatalf("refusal = %v, want reason %s", refusal, tc.reason)
			}
			if run.mutations != tc.mutations {
				t.Fatalf("mutations = %d, want %d", run.mutations, tc.mutations)
			}
			posts := 0
			for _, call := range run.calls {
				if strings.HasPrefix(call, "api -X POST") || strings.HasPrefix(call, "pr comment") {
					posts++
				}
			}
			if wantPosts := map[bool]int{true: 1, false: 0}[tc.post || tc.mutations > 0]; posts != wantPosts {
				t.Fatalf("post attempts = %d, want %d (never retried, never a replacement): %v", posts, wantPosts, run.calls)
			}
			if tc.env[0] == graphqlBlocked && run.called("pr comment") {
				t.Fatalf("a failed REST round fell back to a GraphQL-backed post: %v", run.calls)
			}
			if name == "lookup fails but GraphQL works" && run.called("api repos/") {
				t.Fatalf("a failure while GraphQL works must not reach REST: %v", run.calls)
			}
		})
	}
}

// A sandbox that blocks GraphQL and appends its footer: the round renders,
// publishes once and confirms; the Go predicates read the live comment as an
// approving publication for that head only.
func TestBlindRoundInCombinedSandboxConfirmsAndApproves(t *testing.T) {
	prompt := runPromptHelper(t, "Closes #42", graphqlBlocked)
	if prompt.err != nil || !strings.Contains(prompt.stdout, "Acceptance criterion from issue 42") {
		t.Fatalf("prompt not rendered over REST: %v\n%s", prompt.err, prompt.stderr)
	}
	live := strings.TrimRight(BlindBody("## Decision\nAPPROVED", "abc"), "\n") + liveHostBlock
	run := runPublishHelper(t, &live, graphqlBlocked)
	if run.err != nil || run.mutations != 1 {
		t.Fatalf("combined round: err=%v mutations=%d\n%s", run.err, run.mutations, run.stderr)
	}
	if envelope := run.envelope(t); envelope["state"] != "confirmed" || envelope["verdict"] != "approved" {
		t.Fatalf("unexpected envelope: %v", envelope)
	}
	if verdict, published := PublishedVerdict(live); !AnswersBlind(live, "abc") || !published || verdict != Approved {
		t.Fatalf("Go predicates reject the confirmed live comment: %q %t", verdict, published)
	}
	if AnswersBlind(live, "def") {
		t.Fatal("an approval for another head must stay stale")
	}
}
