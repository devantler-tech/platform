# Complete native source scanning

The adapter checks the complete native Grype 0.118.0 directory input with the
existing source exclusions and base configuration. Analysis and scanning read
one private checkout of the exact Git commit. Original checkout changes cannot
alter that input, and tracked files are verified before and after scanning.

Two advisory-specific proofs may produce versioned native VEX. The CEL proof
inspects every source variant in the fixture owners and their complete selected
module sources. The ACR proof uses native govulncheck 1.8 with all first-party
tests, an explicit Linux/amd64 profile and a private paired test that must make
the affected call reachable. That control is analyzed and never executed.
Fresh canonical advisory data, complete source/module occurrences, compiler and
module checksums bind each decision. Changed premises refuse the scan.

The adapter retains the complete native JSON, including adjudicated findings,
and checks every ignored artifact location against the proven occurrence set.
Every remaining native finding blocks, including UNKNOWN severities below
Grype's native failure thresholds. Full proof receipts are written outside the
scanned checkout under the existing ignored native report directory.

With the reviewed tools installed, run
`bash scripts/grype-source-analysis.sh dir:.` from a committed checkout.
