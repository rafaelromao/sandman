package batch

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
)

// lookupOpenPRFn is the indirection used by LookupOpenPR to invoke the
// underlying `gh pr list` subprocess. Tests inject a fake to avoid
// shelling out. The wire shape is `gh pr list --head <branch> --state
// open --json number,mergeable --limit 1`, whose JSON output is a list of
// `{"number": int, "mergeable": string}` objects.
var lookupOpenPRFn = defaultLookupOpenPR

// lookupOpenPRResult mirrors the JSON shape returned by `gh pr list
// --json number,mergeable` for a single entry.
type lookupOpenPRResult struct {
	Number    int    `json:"number"`
	Mergeable string `json:"mergeable"`
}

// LookupOpenPR reports whether the given branch has an open pull
// request (state=open) and, if so, returns its number and the
// `gh`-reported mergeable state (`"MERGEABLE"`, `"CONFLICTING"`, or
// `"UNKNOWN"`).
//
// Merged PRs are out of scope: `gh pr list --state open` filters them
// out, so a merged branch looks identical to a branch with no PR. The
// caller relies on `checkPRMerged` for the merged-path signal.
//
// Errors from `gh` are surfaced to the caller (network blip, missing
// auth, etc.). Callers decide whether to treat the error as a soft pass
// or hard fail.
func LookupOpenPR(branch string) (bool, int, string, error) {
	return lookupOpenPRFn(branch)
}

func defaultLookupOpenPR(branch string) (bool, int, string, error) {
	if strings.TrimSpace(branch) == "" {
		return false, 0, "", nil
	}
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number,mergeable", "--limit", "1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, 0, "", errors.New("gh pr list: " + err.Error() + ": " + strings.TrimSpace(string(out)))
	}
	var rows []lookupOpenPRResult
	if err := json.Unmarshal(out, &rows); err != nil {
		return false, 0, "", errors.New("parse gh pr list output: " + err.Error())
	}
	if len(rows) == 0 {
		return false, 0, "", nil
	}
	row := rows[0]
	return true, row.Number, row.Mergeable, nil
}
