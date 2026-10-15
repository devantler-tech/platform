# Read-only retirement preflight

The **Check Backstage Retirement Preconditions** workflow evaluates the complete
opening retirement preflight without starting cleanup. Use a fresh first dispatch
on current `main`, the exact accepted live artifact revision, and the distinct
confirmation `CHECK BACKSTAGE RETIREMENT ONLY`.

The command's explicit `--check` mode runs the same footprint assessment, complete
eligibility, separated preservation snapshots, unused-journal read and closing
source check as execution mode. It returns before creating the operation receipt,
admission fence or uninstall options. Its write transport also refuses every
mutation, including admission dry-runs, independently of that early return.

`CHECKED` means only those opening predicates passed for that observation. It does
not prove admission controls, a native uninstall, backup restoration, or safe data
retirement. A refusal identifies the fixed preflight stage after the diagnostic
change is integrated; collected values and command errors remain private.

Neither result authorizes execution or clears an existing journal. Never rerun a
consumed retirement attempt to obtain diagnostics. Diagnose a refusal first;
actual retirement remains a separate protected operation with its own confirmation
and fresh source/deployment evidence.
