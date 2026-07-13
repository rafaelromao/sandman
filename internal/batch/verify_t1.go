package batch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

const (
	OutcomeVerified = "verified"
	OutcomeFailed   = "failed"
	OutcomeNoSignal = "no_signal"
)

type VerificationResult struct {
	Outcome string
	ACs     []ACResult
	Digest  string
}

type ACResult struct {
	TestName       string
	Passed         bool
	FlakyRecovered bool
	Output         string
}

var passLineRe = regexp.MustCompile(`--- PASS:\s+(\S+)`)

func VerifyACTraceability(ctx context.Context, sb sandbox.Sandbox, issueBody string) VerificationResult {
	runs := ExtractAcceptanceCriteria(issueBody)
	if len(runs) == 0 {
		return VerificationResult{Outcome: OutcomeNoSignal, ACs: nil, Digest: ""}
	}
	results := make([]ACResult, 0, len(runs))
	var buf bytes.Buffer
	for _, run := range runs {
		ac := runOneAC(ctx, sb, run)
		results = append(results, ac)
		buf.WriteString(ac.Output)
	}
	outcome := aggregate(results)
	digest := sha256Digest(buf.String())
	return VerificationResult{Outcome: outcome, ACs: results, Digest: digest}
}

func runOneAC(ctx context.Context, sb sandbox.Sandbox, run GoTestRun) ACResult {
	passed, output := runOnce(ctx, sb, run)
	if !passed {
		if retryPassed, retryOutput := runOnce(ctx, sb, run); retryPassed {
			return ACResult{
				TestName:       run.TestName,
				Passed:         true,
				FlakyRecovered: true,
				Output:         retryOutput,
			}
		}
	}
	return ACResult{
		TestName: run.TestName,
		Passed:   passed,
		Output:   output,
	}
}

func runOnce(ctx context.Context, sb sandbox.Sandbox, run GoTestRun) (bool, string) {
	var stdout, stderr bytes.Buffer
	_ = sb.Exec(ctx, run.Command, &stdout, &stderr)
	combined := stdout.String() + stderr.String()
	return hasPassFor(combined, run.TestName), combined
}

func hasPassFor(output, testName string) bool {
	if testName == "" {
		return false
	}
	for _, m := range passLineRe.FindAllStringSubmatch(output, -1) {
		if m[1] == testName {
			return true
		}
	}
	return false
}

func aggregate(results []ACResult) string {
	if len(results) == 0 {
		return OutcomeNoSignal
	}
	anyPass := false
	anyFail := false
	for _, ac := range results {
		if ac.Passed {
			anyPass = true
		} else {
			anyFail = true
		}
	}
	if anyFail {
		return OutcomeFailed
	}
	if anyPass {
		return OutcomeVerified
	}
	return OutcomeNoSignal
}

func sha256Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
