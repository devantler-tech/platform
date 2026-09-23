#!/usr/bin/env bash
#
# Proves guard-tracked-exec-bit.sh actually discriminates, in all four directions:
# it FAILS on a directly-invoked script tracked 100644, PASSES once the same file
# is tracked 100755, PASSES when the invocation moves behind an interpreter (so the
# requirement is derived from the invocation rather than pinned to a filename), and
# refuses to report success when it finds nothing to check.
#
# Each fixture is a throwaway git repository. The mode has to be set in the INDEX
# rather than on disk, because that is the only thing the guard reads and the only
# thing a fresh clone receives — a working-tree chmod would prove nothing.

set -euo pipefail

guard="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/guard-tracked-exec-bit.sh"
failures=0

# `anchor` adds a SECOND script that is directly invoked and tracked executable.
# Without it a fixture whose only script is interpreter-invoked has no direct
# invocation at all, so the guard refuses on anti-vacuity and a "relaxed" case
# would pass for the wrong reason — proving nothing about relaxation.
# `target` defaults to scripts/fixture-target.sh; pass a path under
# .github/scripts/ to cover the CI installer surface, which is invoked as the
# command of a run step rather than as ./scripts/<name>.
make_fixture() {
  local dir="$1" mode="$2" invocation="$3" anchor="${4:-no}"
  local target="${5:-scripts/fixture-target.sh}"

  mkdir -p "$dir/scripts" "$dir/.github/workflows" "$dir/$(dirname "$target")"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t

  printf '#!/usr/bin/env bash\necho hi\n' > "$target"
  printf 'jobs:\n  j:\n    steps:\n      - run: %s\n' "$invocation" > .github/workflows/w.yaml

  if [[ "$anchor" == "anchor" ]]; then
    printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
    printf '      - run: ./scripts/fixture-anchor.sh\n' >> .github/workflows/w.yaml
  fi

  git add -A
  git update-index --chmod="$mode" "$target"
  [[ "$anchor" == "anchor" ]] && git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}

expect() {
  local label="$1" want_rc="$2" want_text="$3" dir="$4"

  set +e
  output="$(bash "$guard" "$dir" 2>&1)"
  rc=$?
  set -e

  if [[ "$rc" != "$want_rc" ]]; then
    echo "::error::$label: rc=$rc want=$want_rc — $output"
    failures=$((failures + 1))
    return
  fi
  if [[ "$output" != *"$want_text"* ]]; then
    echo "::error::$label: rc matched but for the wrong reason — $output"
    failures=$((failures + 1))
    return
  fi
  echo "$label ✅"
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# RED: directly invoked, tracked non-executable.
make_fixture "$work/red" -x './scripts/fixture-target.sh'
expect "RED   directly-invoked 100644 is rejected" 1 "is tracked 100644, not 100755" "$work/red"

# GREEN: the same invocation, with the bit tracked.
make_fixture "$work/green" +x './scripts/fixture-target.sh'
expect "GREEN directly-invoked 100755 is accepted" 0 "exec-bit guard OK" "$work/green"

# RELAXATION: the requirement follows the invocation. The identical non-executable
# file is fine once an interpreter runs it, so the guard is not simply always-on.
make_fixture "$work/relaxed" -x 'bash ./scripts/fixture-target.sh' anchor
expect "RELAX interpreter-invoked 100644 is accepted" 0 "exec-bit guard OK" "$work/relaxed"

# ANTI-VACUITY: no direct invocation anywhere is cannot-check, never a clean pass.
make_fixture "$work/empty" -x 'echo nothing-to-see'
expect "VACUOUS no invocation at all refuses to pass" 1 "examined nothing" "$work/empty"

# UNRESOLVABLE RELATIVE INVOCATION (#3688): a step's `working-directory:` (or a
# `cd` earlier in its run block) changes what a relative path names, so
# `./fixture-target.sh` run from `scripts` execs the tracked
# scripts/fixture-target.sh — but its text matches no path the guard models. The
# anchor satisfies anti-vacuity, so the guard used to report success over a
# 100644 script the runner genuinely execs. It now fails closed, naming the
# invocation it could not resolve.
make_workdir_fixture() {
  local dir="$1" workdir="$2" invocation="$3"
  mkdir -p "$dir/scripts/tests" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n'
    printf '      - working-directory: %s\n' "$workdir"
    printf '        run: %s\n' "$invocation"
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_workdir_fixture "$work/workdir" scripts './fixture-target.sh'
expect "WORKDIR relative invocation from a working-directory fails closed" 1 \
  "cannot resolve './fixture-target.sh'" "$work/workdir"

make_workdir_fixture "$work/workdir-parent" scripts/tests '../fixture-target.sh'
expect "WORKDIR parent-relative invocation fails closed" 1 \
  "cannot resolve '../fixture-target.sh'" "$work/workdir-parent"

make_workdir_fixture "$work/workdir-cd" . 'cd scripts && ./fixture-target.sh'
expect "WORKDIR relative invocation after a cd fails closed" 1 \
  "cannot resolve './fixture-target.sh'" "$work/workdir-cd"

# ...but only when the path is EXECED. The same relative path handed to an
# interpreter needs no execute bit, so failing it would be "flag every relative
# path" — a false positive that fails every PR and merge-group run.
make_workdir_fixture "$work/workdir-bash" scripts 'bash ./fixture-target.sh'
expect "WORKDIR relative path handed to an interpreter stays accepted" 0 \
  "exec-bit guard OK" "$work/workdir-bash"

# ...and only when it could name a tracked script. A relative path matching no
# tracked *.sh is out of scope exactly as an untracked `./scripts/` path is: it
# is overwhelmingly a string a test builds — this file carries several — and a
# genuinely missing script fails loudly on its first run anyway.
make_workdir_fixture "$work/workdir-untracked" scripts './not-a-tracked-script.sh'
expect "WORKDIR relative path naming no tracked script is out of scope" 0 \
  "exec-bit guard OK" "$work/workdir-untracked"


# .GITHUB/SCRIPTS COVERAGE: the CI installers live under .github/scripts and are
# invoked as the command of a run step, with no ./ in front. That surface was
# outside both the mode inventory and the detector, so a mode-only regression on
# setup-ksail.sh or setup-talosctl.sh was invisible while the guard still
# reported success over unrelated root scripts.
make_fixture "$work/dotgithub" -x '.github/scripts/fixture-target.sh' no \
  '.github/scripts/fixture-target.sh'
expect "GHDIR .github/scripts 100644 is rejected" 1 "is tracked 100644, not 100755" "$work/dotgithub"

# QUOTED INVOCATION: the shell execs a quoted path exactly as it execs a bare
# one, so it needs the bit; a delimiter class that omits quotes silently skips it.
make_fixture "$work/quoted" -x '"./scripts/fixture-target.sh"'
expect "QUOTE quoted direct invocation is rejected" 1 "is tracked 100644, not 100755" "$work/quoted"

# CONTINUATION LINE IS NOT COMMAND POSITION: recognising a run block's command by
# line-leading position must not also claim every backslash-continued ARGUMENT.
# This repository continues a shellcheck call over library files tracked 100644,
# and counting those fails the build over files nothing execs. The anchor keeps
# the sweep non-vacuous so rc=0 means "not claimed", never "nothing examined".
make_continuation_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho lib\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n      - run: |\n'
    printf '          shellcheck \\\n'
    printf '            scripts/fixture-target.sh\n'
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_continuation_fixture "$work/continuation"
expect "CONT  continued argument is not an invocation" 0 "exec-bit guard OK" "$work/continuation"

# RUN-BLOCK COMMAND: a multi-line run block names the command on its own line
# with no ./ and no run: keyword on that line — the shape the talosctl installer
# uses. Neither the delimiter pass nor the run: pass can see it.
make_runblock_fixture() {
  local dir="$1" mode="$2"
  mkdir -p "$dir/scripts" "$dir/.github/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > .github/scripts/fixture-target.sh
  {
    printf 'jobs:\n  j:\n    steps:\n      - run: |\n'
    printf '          .github/scripts/fixture-target.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod="$mode" .github/scripts/fixture-target.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_runblock_fixture "$work/runblock" -x
expect "BLOCK run-block command 100644 is rejected" 1 "is tracked 100644, not 100755" "$work/runblock"
make_runblock_fixture "$work/runblock-ok" +x
expect "BLOCK run-block command 100755 is accepted" 0 "exec-bit guard OK" "$work/runblock-ok"

# QUOTED PATH FILTER IS NOT AN INVOCATION: a paths-filter entry is a quoted BARE
# path. Honouring a quote in front of a bare path would claim every such entry —
# this repository has one on a file correctly tracked 100644 — so the quote
# counts only in front of ./ . The anchor keeps the sweep non-vacuous.
make_filter_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho lib\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'on:\n  pull_request:\n    paths:\n'
    printf "      - 'scripts/fixture-target.sh'\n"
    printf 'jobs:\n  j:\n    steps:\n      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_filter_fixture "$work/filter"
expect "FILTER quoted path-filter entry is not an invocation" 0 "exec-bit guard OK" "$work/filter"

# .GITHUB BEYOND .GITHUB/SCRIPTS: a composite action ships its own scripts, and
# `.github/actions/<name>/check.sh` is executed by a run step exactly as an
# installer under .github/scripts is. Inventorying only .github/scripts left that
# path outside BOTH the mode inventory and the path regex, so the guard could not
# find its mode and reported success over it while another invocation kept the
# sweep non-vacuous.
make_fixture "$work/ghany" -x '.github/actions/example/check.sh' anchor \
  '.github/actions/example/check.sh'
expect "GHANY .github/actions 100644 is rejected" 1 "is tracked 100644, not 100755" "$work/ghany"

# QUOTED BARE PATH IN A RUN STEP: YAML strips the quotes before the shell sees the
# value, so `run: "scripts/x.sh"` is the same command as `run: scripts/x.sh` and
# needs the bit. The quote allowance stays scoped to `run:` — a `paths:` entry is
# also a quoted bare path and must keep being excluded, which FILTER below pins.
make_fixture "$work/qbare" -x '"scripts/fixture-target.sh"' anchor
expect "QBARE quoted bare run: command is rejected" 1 "is tracked 100644, not 100755" "$work/qbare"

# CONTROL KEYWORD IS NOT A CONSUMING COMMAND: `if ./scripts/x.sh; then` execs the
# file; `if` introduces a command position rather than taking it as an argument.
# It was landing in the catch-all beside genuine argument-takers and being skipped.
make_fixture "$work/ctrl" -x 'if ./scripts/fixture-target.sh; then echo ok; fi' anchor
expect "CTRL  if-prefixed invocation is rejected" 1 "is tracked 100644, not 100755" "$work/ctrl"

# ...and the same three surfaces must still RELAX behind an interpreter, so the new
# coverage is derived from the invocation rather than pinned to a path or keyword.
make_fixture "$work/ctrl-relaxed" -x 'if bash ./scripts/fixture-target.sh; then echo ok; fi' anchor
expect "CTRL  if + interpreter is accepted" 0 "exec-bit guard OK" "$work/ctrl-relaxed"

# MULTI-WORD PREFIX IN FRONT OF A BARE PATH: extraction used to allow at most ONE
# word before a path, and only a `./`-prefixed path could carry more. A bare path
# behind two or more words — an environment assignment, or a wrapper with an
# option — therefore never reached the prefix classifier at all, and the guard
# reported success over a 100644 file the step really does exec. Review found
# these one spelling at a time over several rounds, which is why extraction is now
# permissive and the classifier is the whitelist that decides.
make_fixture "$work/envassign" -x 'env FOO=1 scripts/fixture-target.sh' anchor
expect "PREFIX env assignment before bare path is rejected" 1 "is tracked 100644, not 100755" "$work/envassign"

make_fixture "$work/bareassign" -x 'FOO=1 scripts/fixture-target.sh' anchor
expect "PREFIX bare assignment before bare path is rejected" 1 "is tracked 100644, not 100755" "$work/bareassign"

make_fixture "$work/wrapopt" -x 'sudo -E scripts/fixture-target.sh' anchor
expect "PREFIX wrapper option before bare path is rejected" 1 "is tracked 100644, not 100755" "$work/wrapopt"

# ...and the widened extraction must still RELAX. The same multi-word prefix ending
# in an interpreter hands the file over rather than execing it. Without this the
# three assertions above would pass just as well on a classifier that accepted
# everything it now reaches, which would prove nothing about discrimination.
make_fixture "$work/envinterp" -x 'env FOO=1 bash scripts/fixture-target.sh' anchor
expect "PREFIX env assignment + interpreter is accepted" 0 "exec-bit guard OK" "$work/envinterp"

# A BARE `-` IS THE YAML SEQUENCE MARKER, NOT A COMMAND: an UNQUOTED paths-filter
# entry is a list item, and the widened extraction now reaches it where the quoted
# form above is excluded by its quote. Only an option to a wrapper already accepted
# may sit in front of the path; a lone `-` may not.
make_unquoted_filter_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho lib\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'on:\n  pull_request:\n    paths:\n'
    printf '      - scripts/fixture-target.sh\n'
    printf 'jobs:\n  j:\n    steps:\n      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_unquoted_filter_fixture "$work/filter-unquoted"
expect "FILTER unquoted path-filter entry is not an invocation" 0 "exec-bit guard OK" "$work/filter-unquoted"

# LINE-LEADING BARE PATH WITH A TRAILING ARGUMENT: the run-block scan used to emit
# the WHOLE line, while the prefix strip anchors the path at end-of-string. An
# argument after the path defeated that anchor, so the prefix became the entire
# line, hit the catch-all and was discarded — the guard skipped a script the step
# genuinely execs. This is not hypothetical: `ci.yaml` runs
# `scripts/update-vendored-operators.sh --validate-committed` in exactly this
# shape, and the pre-fix guard did not check it. Extraction now stops at the path.
# The fixture must use a run BLOCK: as a `run:` line the path is already the end
# of the `run:` match, so the anchor holds and the case proves nothing.
make_trailarg_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n      - run: |\n'
    printf '          scripts/fixture-target.sh --some-flag\n'
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_trailarg_fixture "$work/trailarg"
expect "TRAIL run-block bare path with trailing argument is rejected" 1 "is tracked 100644, not 100755" "$work/trailarg"
# A quoted environment assignment is still a direct exec: the shell sets the
# variable and execs the file. The prefix classifier reads NAME=value as
# transparent, so this case is about whether extraction hands it the occurrence
# at all — a character-class TOKEN_RE excluding `"`, `$` and braces did not.
# shellcheck disable=SC2016 # the literal ${BAR} is the case under test; it must not expand
make_fixture "$work/quoted-assign" -x 'FOO="${BAR}" scripts/fixture-target.sh' anchor
expect "QUOTED-ASSIGN quoted env assignment before a bare path is rejected" 1 \
  "is tracked 100644, not 100755" "$work/quoted-assign"

# The counterpart: the same assignment in front of an INTERPRETER is not a direct
# exec, so widening extraction must not have made the guard accept everything.
# shellcheck disable=SC2016 # same: the literal is the fixture
make_fixture "$work/quoted-assign-bash" -x 'FOO="${BAR}" bash scripts/fixture-target.sh' anchor
expect "QUOTED-ASSIGN interpreter behind a quoted assignment stays accepted" 0 \
  "" "$work/quoted-assign-bash"


# CHAINED COMMAND: `cmd && ./scripts/x.sh` puts a SECOND command position after the
# separator, and the walk has to notice. Extraction is greedy, so the occurrence
# begins at `echo`; the classifier hit that unknown token, broke immediately and
# discarded the whole occurrence, never reaching the `&&` that proves a fresh
# command position follows. With the anchor satisfying anti-vacuity the guard then
# reported success over a script it genuinely execs. The walk now RESETS at a
# separator instead of judging the occurrence by its first segment.
make_fixture "$work/chained" -x 'echo ready && ./scripts/fixture-target.sh' anchor
expect "CHAIN direct invocation after a command separator is rejected" 1 \
  "is tracked 100644, not 100755" "$work/chained"

# The counterpart, so the reset cannot become "accept whatever follows a
# separator": an INTERPRETER after the separator is still not a direct exec.
make_fixture "$work/chained-bash" -x 'echo ready && bash ./scripts/fixture-target.sh' anchor
expect "CHAIN interpreter after a command separator stays accepted" 0 \
  "" "$work/chained-bash"

# ASSIGNMENT VALUE: `HELPER="./scripts/x.sh"` stores a path, it does not run one.
# The opening quote is a leading delimiter, so extraction started AT the quote and
# dropped the `HELPER=` that gives it its meaning; the classifier then saw a lone
# `"`, read it as an operator and required an execute bit on a file that is only
# ever handed to an interpreter. That is a false POSITIVE — it fails every PR and
# merge-group run — so the fixture asserts the guard passes.
make_assign_value_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf '#!/usr/bin/env bash\n'
    printf 'HELPER="./scripts/fixture-target.sh"\n'
    # shellcheck disable=SC2016 # the literal $HELPER is the fixture; it must not expand
    printf 'bash "$HELPER"\n'
  } > scripts/fixture-caller.sh
  printf 'jobs:\n  j:\n    steps:\n      - run: ./scripts/fixture-anchor.sh\n' > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git update-index --chmod=+x scripts/fixture-caller.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_assign_value_fixture "$work/assign-value"
expect "ASSIGN-VALUE a path stored in a quoted assignment is not an invocation" 0 \
  "" "$work/assign-value"

# THE MIRROR IMAGE OF CHAIN, and the reason the split had to happen in EXTRACTION
# rather than only in the classifier. Here the swallowed segment is the one that
# matters: `grep -o` takes the longest leftmost match, so both paths land in one
# occurrence ending at the interpreter-invoked path, the classifier judges it by
# that `bash` prefix, and the genuinely-execed FIRST path never becomes an
# occurrence of its own. A classifier-only reset still reports success here.
make_chain_first_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho a\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho b\n' > scripts/fixture-other.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n'
    printf '      - run: ./scripts/fixture-target.sh && bash ./scripts/fixture-other.sh\n'
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=-x scripts/fixture-other.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_chain_first_fixture "$work/chain-first"
expect "CHAIN-FIRST a direct invocation BEFORE a separator is still checked" 1 \
  "'scripts/fixture-target.sh' is invoked directly" "$work/chain-first"

# GLUED SEMICOLON: bash needs no space before `;`, so `echo ready; scripts/x.sh`
# yields the prefix word `ready;`. The classifier's `;` arm matches a SEPARATE
# token only, so the word hit the catch-all and the occurrence was discarded —
# a fail-open, since anti-vacuity was satisfied by the anchor. `;` is now split
# into its own word before the walk.
make_fixture "$work/semi-line" -x 'echo ready; scripts/fixture-target.sh' anchor
expect "SEMI glued semicolon before a bare path is rejected" 1 \
  "is tracked 100644, not 100755" "$work/semi-line"

# The interpreter control, so the split cannot become "accept whatever follows a
# semicolon".
make_fixture "$work/semi-bash" -x 'echo ready; bash scripts/fixture-target.sh' anchor
expect "SEMI interpreter after a glued semicolon stays accepted" 0 \
  "" "$work/semi-bash"

# The same shape inside a `run: |` block, which reaches the guard through the
# line-leading branch rather than the `run:` branch — a separate extraction path,
# so it needs its own fixture rather than being assumed equivalent.
make_semi_block_fixture() {
  local dir="$1"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n      - run: |\n'
    printf '          echo ready; scripts/fixture-target.sh\n'
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_semi_block_fixture "$work/semi-block"
expect "SEMI glued semicolon inside a run block is rejected" 1 \
  "is tracked 100644, not 100755" "$work/semi-block"

# GLUED SEPARATORS (#3693): bash needs no whitespace around `|`, `&&` or `;`, so a
# separator can be glued to the word before it, or to the path after it. Each
# spelling below used to leave the guard reporting success over a 100644 script
# the step genuinely execs: `ready|` and `ready&&` are single words that no
# separator arm or segmentation split recognised, and with nothing at all after
# the `;` extraction produced no occurrence to judge. Separators are now split out
# by one quote-aware pass before extraction, so no stage has to learn a spelling.
make_fixture "$work/glued-pipe" -x 'echo ready| scripts/fixture-target.sh' anchor
expect "GLUED pipe glued to the preceding word is rejected" 1 \
  "is tracked 100644, not 100755" "$work/glued-pipe"

make_fixture "$work/glued-and" -x 'echo ready&& scripts/fixture-target.sh' anchor
expect "GLUED && glued to the preceding word is rejected" 1 \
  "is tracked 100644, not 100755" "$work/glued-and"

make_fixture "$work/glued-semi-path" -x 'echo ready;scripts/fixture-target.sh' anchor
expect "GLUED semicolon glued to both sides is rejected" 1 \
  "is tracked 100644, not 100755" "$work/glued-semi-path"

# The interpreter controls, so splitting a separator out cannot become "demand
# the bit on whatever follows a separator".
make_fixture "$work/glued-pipe-bash" -x 'echo ready| bash scripts/fixture-target.sh' anchor
expect "GLUED interpreter after a glued pipe stays accepted" 0 \
  "exec-bit guard OK" "$work/glued-pipe-bash"

make_fixture "$work/glued-semi-bash" -x 'echo ready;bash scripts/fixture-target.sh' anchor
expect "GLUED interpreter after a fully glued semicolon stays accepted" 0 \
  "exec-bit guard OK" "$work/glued-semi-bash"

# A SEPARATOR INSIDE QUOTES IS TEXT, NOT A COMMAND BOUNDARY: `echo "done;"` hands
# echo one argument, so the path after it is still echo's argument. A split that
# ignored quotes would manufacture a command position there — a false positive,
# which fails every PR and merge-group run — so these must stay accepted.
make_fixture "$work/quoted-semi" -x 'echo "done;" scripts/fixture-target.sh' anchor
expect "QUOTED-SEP a semicolon inside double quotes is not a separator" 0 \
  "exec-bit guard OK" "$work/quoted-semi"

make_fixture "$work/quoted-pipe" -x "echo 'a |' scripts/fixture-target.sh" anchor
expect "QUOTED-SEP a pipe inside single quotes is not a separator" 0 \
  "exec-bit guard OK" "$work/quoted-pipe"

# ...while a REAL separator after a quoted one still opens a command position.
make_fixture "$work/quoted-then-real" -x 'echo "a; b" && scripts/fixture-target.sh' anchor
expect "QUOTED-SEP a real && after a quoted semicolon is rejected" 1 \
  "is tracked 100644, not 100755" "$work/quoted-then-real"

# A REDIRECTION IS NOT A SEPARATOR: the `&` in `2>&1` must not be split like
# `&&`, or the path after it would read as a fresh command.
make_fixture "$work/redirect" -x 'cmd 2>&1 scripts/fixture-target.sh' anchor
expect "REDIRECT 2>&1 does not open a command position" 0 \
  "exec-bit guard OK" "$work/redirect"

# ...but a LONE `&` backgrounds the command before it, and what follows is a new
# command that the shell execs.
make_fixture "$work/background" -x 'sleep 1 & scripts/fixture-target.sh' anchor
expect "BACKGROUND a direct invocation after a lone & is rejected" 1 \
  "is tracked 100644, not 100755" "$work/background"

# THE MIRROR IMAGE FOR `;` AND `|`: exactly as CHAIN-FIRST pins for `&&`, a greedy
# match swallowed a direct invocation BEFORE the separator into an occurrence
# judged by the interpreter that followed it. Splitting every separator before
# extraction gives the first command its own occurrence.
make_two_script_fixture() {
  local dir="$1" invocation="$2"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho a\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho b\n' > scripts/fixture-other.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n'
    printf '      - run: %s\n' "$invocation"
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=-x scripts/fixture-other.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_two_script_fixture "$work/semi-first" './scripts/fixture-target.sh; bash ./scripts/fixture-other.sh'
expect "SEMI-FIRST a direct invocation BEFORE a semicolon is still checked" 1 \
  "'scripts/fixture-target.sh' is invoked directly" "$work/semi-first"

make_two_script_fixture "$work/pipe-first" './scripts/fixture-target.sh | bash ./scripts/fixture-other.sh'
expect "PIPE-FIRST a direct invocation BEFORE a pipe is still checked" 1 \
  "'scripts/fixture-target.sh' is invoked directly" "$work/pipe-first"

# AN UNPAIRED QUOTE IS TEXT: the guard reads one line at a time, so the closing
# line of a multi-line string carries a quote with no partner on that line.
# Reading it as an OPENING quote hid every separator after it, and the direct
# invocation that follows the string went unchecked. Each line below is the
# second line of `echo "multi` … `line"`, inside one run block.
make_multiline_string_fixture() {
  local dir="$1" closing_line="$2"
  mkdir -p "$dir/scripts" "$dir/.github/workflows"
  cd "$dir"
  git init -q .
  git config user.email t@example.com
  git config user.name t
  printf '#!/usr/bin/env bash\necho hi\n' > scripts/fixture-target.sh
  printf '#!/usr/bin/env bash\necho anchor\n' > scripts/fixture-anchor.sh
  {
    printf 'jobs:\n  j:\n    steps:\n      - run: |\n'
    printf '          echo "multi\n'
    printf '          %s\n' "$closing_line"
    printf '      - run: ./scripts/fixture-anchor.sh\n'
  } > .github/workflows/w.yaml
  git add -A
  git update-index --chmod=-x scripts/fixture-target.sh
  git update-index --chmod=+x scripts/fixture-anchor.sh
  git -c commit.gpgsign=false commit -qm fixture
  cd - > /dev/null
}
make_multiline_string_fixture "$work/unpaired-semi" 'line"; ./scripts/fixture-target.sh'
expect "UNPAIRED a direct invocation after a string's closing quote and ; is rejected" 1 \
  "is tracked 100644, not 100755" "$work/unpaired-semi"

make_multiline_string_fixture "$work/unpaired-and" 'line" && scripts/fixture-target.sh'
expect "UNPAIRED a direct invocation after a string's closing quote and && is rejected" 1 \
  "is tracked 100644, not 100755" "$work/unpaired-and"

make_multiline_string_fixture "$work/unpaired-pipe" 'line"| ./scripts/fixture-target.sh'
expect "UNPAIRED a direct invocation after a string's closing quote and | is rejected" 1 \
  "is tracked 100644, not 100755" "$work/unpaired-pipe"

# The interpreter control, so "an unpaired quote is text" cannot become "demand
# the bit on everything after an unpaired quote".
make_multiline_string_fixture "$work/unpaired-bash" 'line"; bash ./scripts/fixture-target.sh'
expect "UNPAIRED an interpreter after a string's closing quote stays accepted" 0 \
  "exec-bit guard OK" "$work/unpaired-bash"

if ((failures > 0)); then
  echo "::error::$failures exec-bit guard assertion(s) failed"
  exit 1
fi

echo "all exec-bit guard assertions passed"
