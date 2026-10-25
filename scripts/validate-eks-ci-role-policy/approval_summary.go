package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// writeApprovalSummary appends the approved-surface lines an unapproved render
// needs to the GitHub job summary at summaryPath, so a reviewer sees the exact
// approval edit on the pull request instead of only inside a failed log (#4773).
// It approves nothing: the edit stays a reviewed change to the ledger file, and
// the validator still fails. Errors other than a surface mismatch, or an empty
// summaryPath outside GitHub Actions, write nothing.
func writeApprovalSummary(validationErr error, summaryPath string) error {
	var mismatch *surfaceMismatchError
	if summaryPath == "" || !errors.As(validationErr, &mismatch) || len(mismatch.delta) == 0 {
		return nil
	}
	var body strings.Builder
	body.WriteString("### Production authorization approval needed\n\n")
	fmt.Fprintf(&body, "The rendered production authorization surface differs from `%s`. "+
		"If this change is intended, approve it by making exactly these edits to that file "+
		"(`-` remove the line, `+` add it) and committing them for review:\n\n", surfaceLedgerFile)
	body.WriteString("```diff\n")
	for _, line := range mismatch.delta {
		// Ledger lines never contain a backtick; refusing one keeps the fence intact.
		if strings.Contains(line, "`") {
			return fmt.Errorf("ledger line cannot be shown safely: %q", line)
		}
		body.WriteString(line + "\n")
	}
	body.WriteString("```\n")

	file, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // GitHub-provided summary path.
	if err != nil {
		return err
	}
	if _, err := file.WriteString(body.String()); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
