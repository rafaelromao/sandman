package cmd

import "testing"

// TestRenderRunMeta_SuccessEmptyVerdictRendersApproved pins issue #2220:
// when the parent implementation row has reached status='success' (the PR
// was merged, which in Sandman only happens after the GitHub-side
// reviewDecision is APPROVED) and the reviewer agent's local decision.md
// is missing or unparseable (so reviewVerdict is empty), renderRunMeta
// must render "N review(s) - Approved" instead of the misleading
// "Unclear" fallback. The short-circuit reflects the invariant "an
// implementation run cannot reach status=success unless the PR was
// approved".
func TestRenderRunMeta_SuccessEmptyVerdictRendersApproved(t *testing.T) {
	js := `const run = {
  key: 'impl-2220-a', runId: 'impl-2220-a', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 1, reviewVerdict: '', reviewLive: false,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - Approved') < 0) throw new Error('expected "1 review - Approved" when status="success" and reviewVerdict empty (only an approved PR can lead to run success), got ' + JSON.stringify(meta));
if (meta.indexOf('Unclear') >= 0) throw new Error('expected no "Unclear" when the impl run succeeded; PR approval is implied by status=success, got ' + JSON.stringify(meta));
if (meta.indexOf('In Progress') >= 0) throw new Error('expected no "In Progress" when the impl run is terminal, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_SuccessTwoReviewsEmptyVerdictRendersApproved pins the
// plural form of the #2220 short-circuit: reviewCount=2 with empty
// reviewVerdict and status=success renders "2 reviews - Approved".
func TestRenderRunMeta_SuccessTwoReviewsEmptyVerdictRendersApproved(t *testing.T) {
	js := `const run = {
  key: 'impl-2220-b', runId: 'impl-2220-b', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 2, reviewVerdict: '', reviewLive: false,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('2 reviews - Approved') < 0) throw new Error('expected "2 reviews - Approved" when status="success" and reviewVerdict empty, got ' + JSON.stringify(meta));
if (meta.indexOf('2 reviews - Unclear') >= 0) throw new Error('expected no "Unclear" when the impl run succeeded, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_SuccessPopulatedVerdictStillWins pins that the
// populated reviewVerdict branch keeps priority over the success
// short-circuit: a status=success row with reviewVerdict="Changes
// requested" renders "Changes Requested", not "Approved". Defensive:
// this shape (run success with changes-requested verdict) cannot arise
// in normal flow, but the branch ordering must keep the populated
// verdict first so a server-side projection regression cannot silently
// override it.
func TestRenderRunMeta_SuccessPopulatedVerdictStillWins(t *testing.T) {
	js := `const run = {
  key: 'impl-2220-c', runId: 'impl-2220-c', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 1, reviewVerdict: 'Changes requested', reviewLive: false,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - Changes Requested') < 0) throw new Error('expected "1 review - Changes Requested" when reviewVerdict="Changes requested" regardless of status, got ' + JSON.stringify(meta));
if (meta.indexOf('1 review - Approved') >= 0) throw new Error('populated verdict must win over the success short-circuit, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_FailureEmptyVerdictStillRendersUnclear pins that the
// failure-mode Unclear fallback is preserved: a status="failure" or
// status="aborted" parent row with empty reviewVerdict and reviewLive=false
// still renders "N review(s) - Unclear" — only the success status unlocks
// the Approved short-circuit.
func TestRenderRunMeta_FailureEmptyVerdictStillRendersUnclear(t *testing.T) {
	js := `const runFailure = {
  key: 'impl-2220-d', runId: 'impl-2220-d', batchKey: 'parent-batch',
  kind: 'completed', status: 'failure', reviewCount: 1, reviewVerdict: '', reviewLive: false,
};
const metaFailure = helpers.renderRunMeta(runFailure);
if (metaFailure.indexOf('1 review - Unclear') < 0) throw new Error('expected "1 review - Unclear" when status="failure" and reviewVerdict empty (failure-mode signal preserved), got ' + JSON.stringify(metaFailure));
if (metaFailure.indexOf('1 review - Approved') >= 0) throw new Error('Approved short-circuit must NOT fire when status="failure", got ' + JSON.stringify(metaFailure));

const runAborted = {
  key: 'impl-2220-e', runId: 'impl-2220-e', batchKey: 'parent-batch',
  kind: 'completed', status: 'aborted', reviewCount: 1, reviewVerdict: '', reviewLive: false,
};
const metaAborted = helpers.renderRunMeta(runAborted);
if (metaAborted.indexOf('1 review - Unclear') < 0) throw new Error('expected "1 review - Unclear" when status="aborted" and reviewVerdict empty, got ' + JSON.stringify(metaAborted));
if (metaAborted.indexOf('1 review - Approved') >= 0) throw new Error('Approved short-circuit must NOT fire when status="aborted", got ' + JSON.stringify(metaAborted));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}
