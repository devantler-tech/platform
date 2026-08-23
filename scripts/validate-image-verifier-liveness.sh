#!/usr/bin/env bash

# Assert that the node image verifier can enforce AT ALL.
#
# `talos/cluster/verify-first-party-images.yaml` declares ImageVerificationConfig
# rules. Those rules are inert on their own: containerd does not verify images
# itself, it delegates to the `io.containerd.image-verifier.v1.bindir` plugin,
# which executes every binary in a configured `bin_dir`. containerd's documented
# behaviour when that directory is unset, absent, or empty is to permit the pull:
#
#   "If bin_dir does not exist or contains no files, the image verifier does not
#    block image pulls."
#
# So a cluster with perfect rules and no verifier binary reads EXACTLY like a
# compliant one from every surface we routinely check — the rules are present,
# the node config matches the repository, CI is green — while every image pulls
# unverified. That is not hypothetical: it was the live state of this cluster,
# found only by hand (devantler-tech/platform#2856).
#
# This check asserts the ENFORCEMENT PATH, not the configuration. Asserting that
# the rules exist would NOT have caught #2856 — the rules were correct
# throughout. It is deliberately the weaker of the two signals that issue asks
# for; the strong one is a behavioural test that a known-unsigned image is
# actually refused (#3101). This one's value is that it needs no node rollout
# and fails loudly while the verifier is missing.
#
# It reads LIVE NODE STATE on purpose. A variant that read the repository would
# re-create the exact blind spot it exists to close.
#
# SECURITY: the rendered `/etc/cri/conf.d/cri.toml` carries registry
# credentials. Every read of it is piped straight into a key extractor; the
# file's contents are never stored in a variable, echoed, or included in an
# error message. Keep it that way — this script's output goes to CI logs.

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: validate-image-verifier-liveness.sh [--nodes <ip>[,<ip>...]]

Asserts, for every node, that containerd's image-verifier bin_dir is configured,
exists, and holds at least one executable.

Nodes are discovered from the cluster when --nodes/TALOS_NODES is not given, so
autoscaled nodes are covered without anyone maintaining a list.

Environment overrides: TALOSCTL, KUBECTL, TALOS_NODES, CONTAINERD_CONFIG_FILES,
IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS (default 3; how many times discovery re-reads
a node inventory that changed while the fleet was being checked, before giving up)
Exit: 0 every node can enforce; 1 at least one cannot; 2 usage/infrastructure error.
USAGE
  exit 2
}

readonly talosctl_bin="${TALOSCTL:-talosctl}"
readonly kubectl_bin="${KUBECTL:-kubectl}"

nodes_arg="${TALOS_NODES:-}"

# Whether an explicit fleet was REQUESTED is tracked separately from whether the
# list is non-empty. `--nodes "$TALOS_NODES"` with the variable unset expands to
# an empty string: the option WAS supplied and named nothing. Falling back to
# cluster discovery there checks a different fleet than the caller asked for and
# still exits 0 — and explicit addresses are usually explicit precisely because
# the current kube context is not that fleet.
#
# `${VAR+set}` distinguishes set-but-empty from unset, so an exported
# `TALOS_NODES=` is caught too; that is how CI passes the list.
nodes_requested=0
[[ -n "${TALOS_NODES+set}" ]] && nodes_requested=1
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --nodes)
      [[ "$#" -ge 2 ]] || usage
      nodes_arg="$2"
      nodes_requested=1
      shift 2
      ;;
    -h | --help) usage ;;
    *) usage ;;
  esac
done

# Both containerd instances matter: the CRI one pulls workload images, the system
# one pulls Talos' own. A verifier wired into only one leaves the other open.
readonly default_config_files='/etc/cri/conf.d/cri.toml /etc/containerd/config.toml'
read -r -a config_files <<<"${CONTAINERD_CONFIG_FILES:-${default_config_files}}"

fail_infra() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 2
}

# KUBECTL must name a bare binary, never a command with flags: it is invoked
# quoted, so "kubectl --context x" would be looked up as a single executable
# name and fail. Pin the context with KUBECTL_CONTEXT instead — the deploy
# composite pins `--context admin@prod` for the same reason, because the
# restored kubeconfig's current-context is not guaranteed to be prod.
discover_nodes() {
  local json bad_nodes
  local -a context_args=()
  [[ -n "${KUBECTL_CONTEXT:-}" ]] && context_args=(--context "${KUBECTL_CONTEXT}")
  json="$("${kubectl_bin}" "${context_args[@]+"${context_args[@]}"}" get nodes -o json 2>/dev/null)" ||
    fail_infra 'could not list cluster nodes (kubectl get nodes failed)'

  # Every node must map to EXACTLY ONE InternalIP, and those addresses must be
  # unique across the fleet. Both halves are fail-opens the flattening hides:
  #
  #   * a node whose addresses hold only a Hostname or an ExternalIP matches
  #     nothing and the select SUCCEEDS, so that node silently leaves the fleet;
  #   * two nodes publishing the SAME InternalIP — a malformed or stale cloud
  #     registration — emit that address twice, so both passes inspect the same
  #     machine while the other node is never looked at.
  #
  # Either way the remaining nodes report OK and the run prints a green verdict
  # for a fleet it never fully enumerated: the same fail-open this script exists
  # to detect, one layer earlier. `scripts/refresh-flux-ghcr-auth.sh`'s
  # `validate_talos_node_inventory` refuses the same two shapes before it mutates
  # anything, for the same reason.
  #
  # Checked as its own pass so the offending nodes can be NAMED. Deriving it from
  # a jq error instead would either lose the names or push kubectl's output into
  # an error message.
  bad_nodes="$(printf '%s' "${json}" |
    jq -r '
      [.items[] | {
        name: (.metadata.name // "<unnamed node>"),
        uid: ((.metadata.uid // "") | tostring),
        ips: [(.status.addresses // [])[]
              | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
              | .address]
      }] as $nodes
      | ($nodes | map(select(.ips | length != 1))
          | map("\(.name) (has \(.ips | length) InternalIP addresses, expected 1)")),
        ($nodes | map(select(.uid == "" ))
          | map("\(.name) (has no metadata.uid)")),
        ($nodes | map(select(.ips | length == 1))
          | group_by(.ips[0]) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share InternalIP \(.[0].ips[0])")),
        ($nodes | map(select(.uid != "")) | group_by(.uid) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share UID \(.[0].uid)"))
      | .[]
    ' 2>/dev/null)" ||
    fail_infra 'could not parse node addresses from kubectl output'
  if [[ -n "${bad_nodes}" ]]; then
    fail_infra "unusable node inventory — refusing to report a fleet that was only partly enumerated: $(printf '%s' "${bad_nodes}" | tr '\n' ';' | sed 's/;$//')"
  fi

  # Emitted as UID and address together. An address alone cannot tell a node
  # apart from its own replacement: the autoscaler can retire a machine and
  # bring up a new one that reuses the address, and comparing addresses would
  # call that pair identical -- so a machine that was never inspected would be
  # reported as one that passed. The UID is what makes "the same node" mean the
  # same node.
  printf '%s' "${json}" |
    jq -r '
      .items[]
      | . as $node
      | (.status.addresses // [])[]
      | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
      | "\(($node.metadata.uid // "") | tostring)\t\(.address)"
    ' 2>/dev/null ||
    fail_infra 'could not parse node addresses from kubectl output'
}

# `ls` reports a path's EXISTENCE without emitting its contents, so its exit
# status is safe to branch on; `read` emits the whole config, so a failed read
# must never be the thing that tells "absent" apart from "unreachable".
#
# That distinction is load-bearing. Inferring absence from a failed read means a
# node the runner cannot reach — expired talosconfig, network partition, API
# down — looks exactly like a node that simply does not ship that config file.
# Such a node would be skipped silently, and if any OTHER containerd config
# declared a verifier the fleet would pass while nothing had been checked: the
# same fail-open shape this script exists to detect, reintroduced in the
# detector.
# Three-state probe: 'present', 'absent', or 'error'.
#
# A failed `ls` is ambiguous — the path may not be there, or the node may have
# hiccuped — and only the first of those is a verdict. Collapsing both into
# "this node does not ship that config" lets a transient fault silently drop one
# containerd from the check; if the OTHER one is wired up, `configs_present`
# stays nonzero and the node passes with half its image paths never inspected.
#
# Only a CONFIRMED not-found counts as absent, and the discriminator has to be
# the stderr text because talosctl exits 1 for both. `ls` on a path emits only
# that path's name and never file contents, so matching its stderr cannot expose
# the registry credentials that `/etc/cri/conf.d/cri.toml` carries.
path_probe() {
  local node="$1" path="$2" err status=0
  err="$("${talosctl_bin}" -n "${node}" ls "${path}" 2>&1 >/dev/null)" || status=$?
  if [[ "${status}" -eq 0 ]]; then
    printf 'present\n'
    return 0
  fi
  case "${err}" in
    *'no such file or directory'* | *'NotFound'* | *'not found'*) printf 'absent\n' ;;
    *) printf 'error\n' ;;
  esac
}

node_reachable() {
  "${talosctl_bin}" -n "$1" ls / >/dev/null 2>&1
}

# Emits TWO facts about ONE config file, from ONE read:
#
#   disabled=0|1   whether that file switches the image-verifier plugin OFF
#   bin_dir=<path> the bin_dir its image-verifier table declares, empty if none
#
# ONE read, deliberately. These were two functions doing a `talosctl read` each,
# and the pair could straddle a config change: the on-demand fleet workflow uses
# a different concurrency group from deployments, so it can overlap a Talos or
# containerd update. The first read could then see the OLD enabled config while
# the second saw the REPLACEMENT populated bin_dir -- and a replacement that
# disables the plugin without removing its binaries would be reported OK from
# two snapshots that never coexisted. The node UID does not change, so the
# convergence loop never retries it. Two facts about one configuration have to
# come from one view of that configuration.
#
# Scoping, both halves:
#  * `bin_dir` is scoped to the `io.containerd.image-verifier.v1.bindir` table.
#    It is a generic key name, and an unscoped match accepts one from an
#    unrelated plugin's table -- reporting a node as enforcing on the strength
#    of a setting containerd never consults for image verification.
#  * `disabled_plugins` is scoped to TOP-LEVEL keys. It is a root key, and TOML
#    puts root keys before the first table header, so once a `[` header is seen
#    any later match belongs to some other table and must not count. An
#    unscoped match would let an unrelated key disable the check.
#
# The two never contend: root keys precede the first header, table keys follow
# it, so `toplevel` is still 1 for one and already 0 for the other.
#
# The config is piped straight into awk and only the two extracted values are
# ever printed. `/etc/cri/conf.d/cri.toml` carries registry credentials and this
# script's output goes to CI logs, so the file must never be captured -- not
# into a variable, not into a temp file, not into an error message. See the
# SECURITY note at the top. Merging the reads keeps that property: a single
# `talosctl read` still goes straight into awk and never reaches the shell.
#
# awk rather than sed because the table scoping needs state across lines, and
# because awk consumes all input: a `sed ... | head -n 1` pipeline closes the
# pipe early, and under `pipefail` the resulting SIGPIPE is indistinguishable
# from a genuine read failure. For the same reason the verdicts are recorded and
# printed from END rather than printed and exited on.
config_facts_in() {
  local node="$1" file="$2"
  "${talosctl_bin}" -n "${node}" read "${file}" 2>/dev/null | awk '
    BEGIN {
      SQ = sprintf("%c", 39); DQ = sprintf("%c", 34)
      # A PHYSICAL LINE BOUNDARY inside the array, kept distinct from a space that
      # is genuinely part of a value. TOML forbids a raw control character inside a
      # string, so this byte cannot occur in content that containerd would accept.
      #
      # A real newline cannot be used: this awk (BWK, macOS) splits on a newline in
      # split() whatever separator is given, and gawk/mawk in CI differ again. A
      # SPACE was used before and lost the distinction the TOML rules turn on --
      # """ id""" is a different plugin from """<newline>id""", and only the second
      # has its leading character trimmed.
      NLM = sprintf("%c", 1)
      toplevel = 1; in_array = 0; buf = ""; disabled = 0
      want = 0; found = 0; bin_dir = ""
    }
    # Removes a TOML comment, respecting quoted strings: a # opens a comment only
    # OUTSIDE a string. The lines of a multiline array are concatenated into one
    # buffer below, so a comment left in place swallows what follows it -- the
    # entry on the next line lands inside the comment text, fails the quote test
    # in verdict(), and is skipped. The node then reads as ENABLED while
    # containerd has the verifier DISABLED, which is the false OK this whole
    # checker exists to prevent. A ] inside a comment would also end the array
    # early, so stripping has to happen before that test too.
    #
    # One quote-aware walk answers both questions, rather than a growing list of
    # comment spellings to special-case. A # INSIDE a quoted entry is content:
    # truncating there would drop every entry after it on the same line, which
    # is the same false OK pointing the other way.
    # A TOML MULTILINE string opens and closes with THREE quotes. Reading each quote
    # as its own single-char delimiter toggles the string state on and straight back
    # off across the opener, so the body is then scanned as if it were unquoted: a #
    # inside it truncates the array and a ] inside it ends it, dropping every later
    # entry -- ours among them -- and reporting OK while the verifier is off. Answers
    # how many characters the delimiter at i occupies.
    function delim_len(line, i, c) {
      if (substr(line, i, 3) == c c c) return 3
      return 1
    }
    # QUOTE STATE IS CARRIED ACROSS PHYSICAL LINES, in globals rather than locals. A
    # multiline string may legitimately span lines -- TOML trims a newline that comes
    # straight after the opener, so
    #
    #   disabled_plugins = ["""
    #   io.containerd.image-verifier.v1.bindir"""]
    #
    # names our plugin exactly. Restarting the walk on each line read the closing
    # delimiter on the second line as a fresh OPENER, and the entry compared as
    # something that is not our id -- reporting OK with the verifier off.
    #
    # Only a TRIPLE delimiter carries: a single-line string left unterminated at the
    # end of a line is invalid TOML, and carrying it would swallow the rest of the
    # array on a typo. So the state is dropped unless it is a multiline one.
    function reset_scan_state() {
      sc_instr = 0; sc_q = ""; sc_qlen = 1
      ca_instr = 0; ca_q = ""; ca_qlen = 1
      # The ARRAY path owns the lines from here to the closing bracket and carries its
      # own quote state, so the top-level tracker must let go. Its opener was counted
      # on this same line one rule earlier; leaving it set would swallow every line
      # after the array -- including the verifier table -- as string content.
      ml_open = ""
    }
    function strip_comment(line,   out, i, c, n, sc_run) {
      n = length(line); out = ""
      for (i = 1; i <= n; i++) {
        c = substr(line, i, 1)
        if (sc_instr) {
          if (sc_qlen == 3) {
            # Multiline BASIC strings take escapes too, so an escaped quote does not
            # begin the closing triple.
            if (c == "\\" && sc_q == DQ) { out = out c; i++; out = out substr(line, i, 1); continue }
            # Consume the WHOLE run. TOML reads the leading one or two quotes of a
            # four- or five-quote run as CONTENT and the last three as the delimiter, so
            # stopping at the first three leaves a stray quote that re-opens a string --
            # and everything after it, including a real table header, is read as content.
            if (substr(line, i, 3) == sc_q sc_q sc_q) {
              sc_run = 0
              while (substr(line, i + sc_run, 1) == sc_q) sc_run++
              out = out substr(line, i, sc_run); i += sc_run - 1; sc_instr = 0; continue
            }
            out = out c
            continue
          }
          out = out c
          # Basic strings take backslash escapes; literal ones do not.
          if (c == "\\" && sc_q == DQ) { i++; out = out substr(line, i, 1); continue }
          if (c == sc_q) sc_instr = 0
          continue
        }
        if (c == "#") break
        if (c == SQ || c == DQ) {
          sc_instr = 1; sc_q = c; sc_qlen = delim_len(line, i, c)
          if (sc_qlen == 3) { out = out c c c; i += 2; continue }
        }
        out = out c
      }
      if (sc_instr && sc_qlen != 3) sc_instr = 0
      return out
    }
    # A ] INSIDE a quoted TOML string is not the array terminator. index() cannot
    # tell the two apart, so an earlier entry carrying a literal bracket ended the
    # array early and every later entry -- including ours -- was never examined,
    # leaving disabled = 0 and a verdict of OK while containerd had the verifier
    # switched off. That is the fail-open this script exists to prevent, so the
    # terminator is found with the same quote-aware scan strip_comment uses.
    function closes_array(line,   i, c, n, ca_run) {
      n = length(line)
      for (i = 1; i <= n; i++) {
        c = substr(line, i, 1)
        if (ca_instr) {
          if (ca_qlen == 3) {
            if (c == "\\" && ca_q == DQ) { i++; continue }
            # Same whole-run rule as strip_comment above. Closing at the first three left
            # the fourth quote re-opening a string, so the array never terminated and the
            # verifier own table -- which TOML permits directly after, with no explicit
            # [plugins] parent -- was swallowed as string content. Its populated bin_dir
            # was then never seen and a HEALTHY node failed the gate.
            if (substr(line, i, 3) == ca_q ca_q ca_q) {
              ca_run = 0
              while (substr(line, i + ca_run, 1) == ca_q) ca_run++
              i += ca_run - 1; ca_instr = 0
            }
            continue
          }
          if (c == "\\" && ca_q == DQ) { i++; continue }
          if (c == ca_q) ca_instr = 0
          continue
        }
        if (c == SQ || c == DQ) {
          ca_instr = 1; ca_q = c; ca_qlen = delim_len(line, i, c)
          if (ca_qlen == 3) i += 2
          continue
        }
        if (c == "]") return 1
      }
      if (ca_instr && ca_qlen != 3) ca_instr = 0
      return 0
    }
    # Compare ENTRIES, never the raw array text. A substring test over the whole
    # expression reports a node as disabled while the plugin is enabled: a
    # DIFFERENT plugin whose name merely contains ours
    # (example.io.containerd.image-verifier.v1.bindir-extra) matches. Either way
    # a healthy node fails -- a false alarm on the one signal that is meant to
    # mean enforcement is off.
    #
    # No apostrophes in these comments: the whole program is a single-quoted
    # shell string, so one would end it. And close_at, not close, because awk
    # already has a close() builtin.
    # Splits the array body at the commas that are actually SEPARATORS. A comma
    # inside a string is content: go-toml resolves
    #   [ LITERAL-QUOTED other, "io.containerd.image-verifier.v1.bindir" ]
    # to ONE string naming a different plugin, so our verifier stays ENABLED --
    # but splitting the raw text at every comma invented a second entry equal to
    # our id and reported a healthy node as disabled.
    function split_entries(body,   n, i, c, q, qlen, out, run) {
      n = length(body); i = 1; out = 0; q = ""; qlen = 0
      entries[1] = ""; out = 1
      while (i <= n) {
        c = substr(body, i, 1)
        if (q == "") {
          if (c == "," ) { out++; entries[out] = ""; i++; continue }
          if (c == SQ || c == DQ) {
            q = c
            qlen = (substr(body, i, 3) == c c c) ? 3 : 1
            entries[out] = entries[out] substr(body, i, qlen)
            i += qlen
            continue
          }
          entries[out] = entries[out] c; i++
          continue
        }
        # Inside a string. A basic string escapes its delimiter; a literal one
        # cannot contain its own quote at all, so nothing escapes there.
        if (c == "\\" && q == DQ) {
          entries[out] = entries[out] substr(body, i, 2); i += 2; continue
        }
        if (qlen == 3) {
          if (substr(body, i, 3) == q q q) {
            # Consume the WHOLE run. TOML lets one or two quotes sit against the
            # closing delimiter as content, and leaving them behind here would
            # re-open a string and swallow the next real separator.
            run = 0
            while (substr(body, i + run, 1) == q) run++
            entries[out] = entries[out] substr(body, i, run); i += run; q = ""; qlen = 0
            continue
          }
        } else if (c == q) {
          entries[out] = entries[out] c; i++; q = ""; qlen = 0
          continue
        }
        entries[out] = entries[out] c; i++
      }
      return out
    }
    function verdict(text,   body, count, i, entry, quote, close_at, j, c, run) {
      body = text
      sub(/^[^[]*\[/, "", body)
      sub(/\][^]]*$/, "", body)
      count = split_entries(body)
      for (i = 1; i <= count; i++) {
        entry = entries[i]
        # A line boundary around an entry is layout, exactly like a space: an array
        # written one entry per line arrives with a marker on each side.
        gsub("^[ \t" NLM "]+", "", entry); gsub("[ \t" NLM "]+$", "", entry)
        # TOML string values are quoted; an unquoted token is not an entry.
        quote = substr(entry, 1, 1)
        if (quote != SQ && quote != DQ) continue
        if (substr(entry, 1, 3) == quote quote quote) {
          # A MULTILINE entry resolves to exactly the characters of the single-line
          # form -- measured against go-toml, which returns our identifier for
          # """id""", the literal triple, and plain "id" alike. Reading the opener as
          # ONE quote closed the string on its second character and compared an EMPTY
          # entry, so containerd had the verifier off and the node still reported OK.
          entry = substr(entry, 4)
          # TOML TRIMS a newline that comes immediately after the opening delimiter, and
          # ONLY that one; a second newline is content. Trim exactly one LINE BOUNDARY --
          # never a space. A space written on the same line is content, so
          # """ id""" is the plugin " id" and our verifier stays ENABLED; trimming it
          # reported a healthy node as disabled. Dropping a RUN would be wrong the
          # other way: the real value of
          #   disabled_plugins = ["""
          #
          #   io.containerd.image-verifier.v1.bindir"""]
          # begins with a newline and names a different plugin entirely.
          if (substr(entry, 1, 1) == NLM) entry = substr(entry, 2)
          close_at = 0
          j = 1
          while (j <= length(entry)) {
            c = substr(entry, j, 1)
            if (c == "\\" && quote == DQ) { j += 2; continue }
            if (substr(entry, j, 3) == quote quote quote) { close_at = j; break }
            j++
          }
          if (close_at == 0) continue
          # The delimiter is the LAST three quotes of the run, not the first three:
          # TOML reads the leading one or two as content, so """id"""" is the plugin
          # id" -- a DIFFERENT plugin, and our verifier stays enabled. Closing at the
          # first three compared the bare id and failed a healthy node.
          run = 0
          while (substr(entry, close_at + run, 1) == quote) run++
          if (run > 3) close_at = close_at + run - 3
          entry = substr(entry, 1, close_at - 1)
          # Basic strings interpret escapes; literal ones keep the text verbatim, so
          # decoding a literal would alias a DIFFERENT plugin id onto ours.
          if (quote == DQ) entry = decode_basic(entry, 1)
          if (entry == "io.containerd.image-verifier.v1.bindir") disabled = 1
          continue
        }
        entry = substr(entry, 2)
        if (quote == DQ) {
          # A BASIC entry escapes its closing quote, so a plain index() stops early and
          # compares a TRUNCATED value; and its escapes must be decoded before comparing,
          # exactly as for the key above. Our plugin id written with an escape IS our
          # plugin, and containerd switches the verifier off -- comparing the raw text
          # missed it and the node reported OK with no enforcement.
          close_at = 0
          j = 1
          while (j <= length(entry)) {
            c = substr(entry, j, 1)
            if (c == "\\") { j += 2; continue }
            if (c == quote) { close_at = j; break }
            j++
          }
          if (close_at == 0) continue
          entry = decode_basic(substr(entry, 1, close_at - 1), 0)
        } else {
          # LITERAL entries interpret nothing, so the same text here is a DIFFERENT
          # plugin id containerd never matches. Decoding it would alias it onto ours.
          close_at = index(entry, quote)
          if (close_at == 0) continue
          entry = substr(entry, 1, close_at - 1)
        }
        if (entry == "io.containerd.image-verifier.v1.bindir") disabled = 1
      }
    }
    # A continuation of the array is consumed before the header rule below, so a
    # value that happens to start with a bracket cannot be mistaken for a table.
    in_array {
      line = strip_comment($0)
      # awk splits records on a newline ALONE, so under CRLF every line keeps a
      # trailing CR. TOML removes a CRLF line ending exactly as it removes an LF
      # (measured with go-toml, both at the opening delimiter and at a backslash
      # continuation), so a surviving CR made the value compare unequal to our
      # plugin id and the script printed disabled=0 -- a node reporting OK with the
      # verifier switched off. Drop the line ending here and represent the boundary
      # itself, once, below.
      sub(/\r$/, "", line)
      buf = buf NLM line
      if (closes_array(line)) { in_array = 0; verdict(buf) }
      next
    }
    # A MULTILINE STRING SPANS PHYSICAL LINES, and its content is not TOML syntax.
    # The header rule below fires on any line starting with [ and clears root scope
    # PERMANENTLY, so a line of string content beginning with [ ended root scope and
    # the real disabled_plugins underneath it was never examined -- disabled stayed 0
    # while the verifier table still supplied a populated bin_dir, and the script
    # reported OK with containerd having the plugin switched off. A fail-open.
    #
    # Runs after the array rule, so array continuation lines are consumed there and
    # never reach this tracker; they carry their own state.
    function ml_track(line,   i, n, c, run) {
      n = length(line)
      for (i = 1; i <= n; i++) {
        c = substr(line, i, 1)
        if (ml_open != "") {
          if (c == "\\" && ml_open == DQ) { i++; continue }
          if (substr(line, i, 3) == ml_open ml_open ml_open) {
            run = 0
            while (substr(line, i + run, 1) == ml_open) run++
            i += run - 1
            ml_open = ""
          }
          continue
        }
        # Outside a string a # opens a comment, so nothing after it is syntax.
        if (c == "#") return
        if (c == SQ || c == DQ) {
          if (substr(line, i, 3) == c c c) { ml_open = c; i += 2; continue }
          # A SINGLE-line string cannot span lines, so it only has to be skipped
          # over here -- leaving it open would swallow the rest of the file.
          i++
          while (i <= n) {
            if (substr(line, i, 1) == "\\" && c == DQ) { i += 2; continue }
            if (substr(line, i, 1) == c) break
            i++
          }
        }
      }
    }
    {
      if (ml_open != "") { ml_track($0); next }
      ml_track($0)
    }
    /^[ \t]*\[/ {
      toplevel = 0
      if (!found) {
        header = $0
        gsub(/[ \t]/, "", header)
        gsub(SQ, "", header)
        gsub(DQ, "", header)
        want = (header == "[plugins.io.containerd.image-verifier.v1.bindir]")
      }
      next
    }
    # TOML treats a bare key and a quoted key as the SAME key only when they spell
    # the SAME characters, so disabled_plugins and its quoted forms name one
    # setting. A bare-key-only match never reaches verdict() on the quoted
    # spellings, leaving disabled = 0 while containerd has the verifier switched
    # OFF -- a node reporting OK with no enforcement.
    #
    # NORMALISING by deleting whitespace and quotes went too far the other way: it
    # ALIASED DISTINCT keys onto ours. A key spelled with an interior space inside
    # quotes is a different setting that containerd never reads, and deleting that
    # space failed a node whose verifier was enabled -- a false alarm on the one
    # signal meant to mean enforcement is off. So parse exactly ONE key and strip
    # only its outer delimiters, preserving every character between them.
    #
    # Returns "" when the line is not a simple assignment -- including a DOTTED
    # key, which names a setting inside a table rather than the root one. Sets
    # KEYPOS to the index just past the =, so the caller never has to search for
    # it and cannot be misled by an = inside the key text.
    # A TOML BASIC (double-quoted) string INTERPRETS escapes, so a key written as
    # "disabled_plugins" IS disabled_plugins and containerd switches the verifier
    # off. Comparing the raw text missed it and the node reported OK with no enforcement
    # -- a FAIL-OPEN, the direction that matters for this check.
    #
    # LITERAL (single-quoted) strings interpret nothing: measured with go-toml, the same
    # text in single quotes stays the DISTINCT key disabled_plugins. Decoding it too
    # would alias a key containerd never reads back onto ours, which is the
    # over-normalisation this parser was written to remove. So decoding is scoped to the
    # double-quoted form only.
    #
    # The target is pure ASCII, so an escape decoding outside ASCII can never spell it;
    # such a key simply does not match, which is the safe direction.
    function hexval(h,   i, n, c, v, digits) {
      digits = "0123456789abcdef"
      v = 0; n = length(h)
      for (i = 1; i <= n; i++) {
        c = tolower(substr(h, i, 1))
        v = v * 16 + (index(digits, c) - 1)
      }
      return v
    }
    # `multiline` is 1 only for a TRIPLE-quoted basic string. The line-continuation
    # rule below exists only there: in a single-line basic string a backslash before
    # whitespace is not valid TOML at all, so applying it everywhere would decode a
    # malformed value into our plugin id and fail a node whose verifier is ENABLED --
    # a false alarm on the one signal meant to mean enforcement is off. Literal
    # strings never reach this function, which is what keeps the triple-literal form
    # a DIFFERENT plugin id, exactly as containerd reads it.
    function decode_basic(s, multiline,   out, i, n, c, d, code, j, cp) {
      n = length(s); out = ""; i = 1
      while (i <= n) {
        c = substr(s, i, 1)
        if (c != "\\") { out = out c; i++; continue }
        d = substr(s, i + 1, 1)
        # A backslash that is the last non-whitespace character on a line removes the
        # line ending AND all whitespace up to the next non-whitespace character. So
        # the run consumed here must CONTAIN a line boundary: a backslash-space with no
        # boundary after it is not a continuation at all (it is invalid TOML, which
        # containerd would refuse to load), and treating it as one would decode a
        # malformed value onto our plugin id and fail a node whose verifier is ENABLED.
        #
        # Without the rule the buffered value kept a literal backslash, compared unequal
        # to our identifier, and the script printed disabled=0 -- reporting OK while
        # containerd had the verifier switched off. Reached only when the backslash is
        # genuinely unescaped: a preceding \\ is consumed as a pair by the branch below
        # before this one is ever tested.
        if (multiline && (d == " " || d == "\t" || d == NLM)) {
          j = i + 1
          while (j <= n && (substr(s, j, 1) == " " || substr(s, j, 1) == "\t")) j++
          if (substr(s, j, 1) == NLM) {
            i = j
            while (i <= n && (substr(s, i, 1) == " " || substr(s, i, 1) == "\t" || substr(s, i, 1) == NLM)) i++
            continue
          }
        }
        if (d == "n")       { out = out "\n"; i += 2 }
        else if (d == "t")  { out = out "\t"; i += 2 }
        else if (d == "r")  { out = out "\r"; i += 2 }
        else if (d == "f")  { out = out sprintf("%c", 12); i += 2 }
        else if (d == "b")  { out = out sprintf("%c", 8); i += 2 }
        else if (d == DQ)   { out = out DQ; i += 2 }
        else if (d == "\\") { out = out "\\"; i += 2 }
        else if (d == "u" || d == "U") {
          code = (d == "u") ? substr(s, i + 2, 4) : substr(s, i + 2, 8)
          if (length(code) == ((d == "u") ? 4 : 8) && code ~ /^[0-9A-Fa-f]+$/) {
            cp = hexval(code)
            # `%c` IS BYTE-ORIENTED in several awks, so a code point above 255 is
            # truncated modulo 256 and ALIASES onto an ASCII character. Measured on the
            # awk shipped with this host: U+10069 emits the single byte 0x69, so a key
            # written as a U+10069 escape followed by "o.containerd.image-verifier.v1.bindir"
            # decoded to exactly the verifier ID, and a config that never names the
            # verifier read as though it did. The same aliasing in the other direction
            # reports a healthy node disabled. Every identifier this script compares
            # against is pure ASCII, so a code point above 127 CANNOT be part of a
            # matching key: emit it as a marker no ASCII key can contain, keeping the
            # code point so two distinct characters never collapse into one.
            if (cp <= 127) out = out sprintf("%c", cp)
            else out = out sprintf("%c%X%c", 1, cp, 1)
            i += (d == "u") ? 6 : 10
          } else { out = out c; i++ }
        }
        else { out = out c; i++ }
      }
      return out
    }
    function root_key(s,   i, n, c, q, out) {
      KEYPOS = 0
      n = length(s)
      i = 1
      while (i <= n && (substr(s, i, 1) == " " || substr(s, i, 1) == "\t")) i++
      c = substr(s, i, 1)
      if (c == SQ || c == DQ) {
        q = c; i++; out = ""
        # In a BASIC string a backslash escapes the next character, so scanning to the
        # next quote would stop early on an escaped one and read a truncated key.
        while (i <= n) {
          c = substr(s, i, 1)
          if (c == q) break
          if (q == DQ && c == "\\" && i < n) { out = out substr(s, i, 2); i += 2; continue }
          out = out c; i++
        }
        if (i > n) return ""
        i++
        if (q == DQ) out = decode_basic(out, 0)
      } else {
        out = ""
        while (i <= n) {
          c = substr(s, i, 1)
          if (c !~ /[A-Za-z0-9_-]/) break
          out = out c; i++
        }
        if (out == "") return ""
      }
      while (i <= n && (substr(s, i, 1) == " " || substr(s, i, 1) == "\t")) i++
      if (substr(s, i, 1) != "=") return ""
      KEYPOS = i + 1
      return out
    }
    toplevel {
      if (root_key($0) == "disabled_plugins") {
        # Fresh state per array, so a malformed earlier one cannot leak an open string.
        reset_scan_state()
        buf = strip_comment(substr($0, KEYPOS))
        sub(/\r$/, "", buf)
        if (closes_array(buf)) { verdict(buf) } else { in_array = 1 }
        next
      }
    }
    !found && want && match($0, /^[ \t]*bin_dir[ \t]*=/) {
      value = substr($0, RSTART + RLENGTH)
      sub(/^[ \t]*/, "", value)
      quote = substr(value, 1, 1)
      if (quote != SQ && quote != DQ) next
      value = substr(value, 2)
      end = index(value, quote)
      if (end == 0) next
      bin_dir = substr(value, 1, end - 1)
      found = 1
    }
    # bin_dir LAST, so a value carrying anything unexpected cannot be mistaken
    # for the disabled marker.
    END {
      print "disabled=" (disabled ? 1 : 0)
      print "bin_dir=" bin_dir
    }
  '
}

# Counts executable entries, skipping the directory's own '.' entry. MODE is
# always column 2; NAME is always last. The LABEL column is empty for some files
# on this cluster, so positional parsing from the left past column 2 is unsafe.
#
# The MODE's first character is the entry TYPE, and it must be checked: a
# directory's mode ('drwxr-xr-x') carries an 'x' too, so counting any entry with
# an 'x' would let a bin_dir holding nothing but a subdirectory report as
# enforcing. containerd executes FILES in bin_dir and does not descend into
# subdirectories, so such a node permits every pull while this check calls it
# healthy — the precise fail-open this script exists to detect. Regular files
# ('-') and symlinks ('l') count; everything else does not.
#
# The listing's exit status is preserved, not discarded. The caller only reaches
# this after the directory was proven to exist, so a failure here means the node
# stopped answering mid-check — which must be an infrastructure error, never the
# "holds no executable" verdict. `awk` on empty input prints 0 quite happily, so
# swallowing the status turns an unreachable node into a confident FAIL line.
count_executables() {
  local node="$1" dir="$2" listing status=0
  listing="$("${talosctl_bin}" -n "${node}" ls -l "${dir}" 2>/dev/null)" || status=$?
  [[ "${status}" -eq 0 ]] ||
    fail_infra "could not list ${dir} on ${node} (the directory exists but talosctl ls failed)"
  printf '%s\n' "${listing}" |
    awk 'NR > 1 && $NF != "." && substr($2, 1, 1) ~ /^[-l]$/ && $2 ~ /x/ { n++ } END { print n + 0 }'
}

directory_exists() {
  local node="$1" dir="$2"
  "${talosctl_bin}" -n "${node}" ls "${dir}" >/dev/null 2>&1
}

# Built with a read loop rather than `mapfile` so the script and its tests run
# on bash 3.2 (macOS) as well as CI's bash 5. A check nobody can run locally is
# a check nobody verifies before shipping.
nodes=()
if [[ "${nodes_requested}" -eq 1 && -z "${nodes_arg}" ]]; then
  printf 'ERROR: --nodes/TALOS_NODES was supplied but names no node — refusing to fall back to cluster discovery, which would check a different fleet than was asked for\n' >&2
  usage
fi

if [[ -n "${nodes_arg}" ]]; then
  # A malformed list ('a,,b', a leading or trailing comma, a shell variable that
  # expanded to nothing) is refused rather than quietly shortened: checking FEWER
  # nodes than were asked for and still exiting 0 is a checker reporting a clean
  # fleet it never looked at — the same silence this script exists to break.
  #
  # Validate the RAW STRING, before splitting. `read -a` discards a trailing
  # empty field, so 'worker-1,' splits to exactly one element and a
  # post-split scan for empty entries cannot see the malformation at all.
  if [[ "${nodes_arg}" == ,* || "${nodes_arg}" == *, || "${nodes_arg}" == *,,* ]]; then
    printf 'ERROR: --nodes/TALOS_NODES has an empty entry (leading, trailing or doubled comma): %s\n' \
      "${nodes_arg}" >&2
    usage
  fi
  IFS=',' read -r -a nodes <<<"${nodes_arg}"
else
  # Discovery runs in a command substitution so its exit status reaches this
  # shell. A process substitution would NOT: `fail_infra` inside it exits only
  # that subshell, the loop keeps whatever partial output jq already emitted,
  # and if those nodes happen to be healthy the script exits 0 having silently
  # dropped the rest of the fleet. jq emitting a valid address and then failing
  # on a malformed one is exactly that case.
  discovered_identities="$(discover_nodes)" ||
    fail_infra 'node discovery failed — refusing to report a fleet that was only partly enumerated'
  while IFS=$'\t' read -r _discovered_uid discovered; do
    [[ -n "${discovered}" ]] || continue
    nodes+=("${discovered}")
  done <<<"${discovered_identities}"
fi

[[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check'

# Verdict for ONE containerd instance on one node. Returns 0 when that instance
# can enforce, 1 when it cannot.
check_config() {
  local node="$1" file="$2" bin_dir status=0 executables exec_status=0

  # ONE read for both facts. Reading twice let the two verdicts describe two
  # different configurations -- see config_facts_in.
  local facts disabled=0
  facts="$(config_facts_in "${node}" "${file}")" || status=$?
  # The file's existence was proven before this call, so a read failure here is
  # an infrastructure fault, never a verdict. Reporting it as "cannot enforce"
  # would be a misdiagnosis; swallowing it would be a fail-open.
  [[ "${status}" -eq 0 ]] ||
    fail_infra "could not read ${file} on ${node} (the file exists but talosctl read failed)"

  # A truncated read is not a verdict either. awk always prints both markers, so
  # their absence means the pipeline did not complete.
  case "${facts}" in
    disabled=*$'\n'bin_dir=*) ;;
    *) fail_infra "could not parse ${file} on ${node} (the read completed but produced no verdict)" ;;
  esac
  disabled="${facts%%$'\n'*}"
  disabled="${disabled#disabled=}"
  bin_dir="${facts#*$'\n'bin_dir=}"

  # Asked FIRST: a disabled plugin makes the rest of this verdict irrelevant.
  # bin_dir could be configured, present and full of executables and containerd
  # would still run none of them, so reporting on the directory before checking
  # whether the plugin is switched on would describe a path that is not taken.
  if [[ "${disabled}" == '1' ]]; then
    printf 'FAIL %s [%s]: the io.containerd.image-verifier.v1.bindir plugin is in disabled_plugins — containerd loads no verifier, so it permits every pull whatever bin_dir says\n' \
      "${node}" "${file}"
    return 1
  fi

  if [[ -z "${bin_dir}" ]]; then
    printf 'FAIL %s [%s]: no bin_dir in the io.containerd.image-verifier.v1.bindir table — the plugin has no directory to run, so containerd permits every pull\n' \
      "${node}" "${file}"
    return 1
  fi

  if ! directory_exists "${node}" "${bin_dir}"; then
    # "The directory is not there" and "the node stopped answering" are the same
    # failed `ls` from here. Only the first is a verdict; reporting the second as
    # one would blame the cluster for a runner-side fault.
    node_reachable "${node}" ||
      fail_infra "cannot reach node ${node} while checking ${bin_dir} (talosctl ls / failed)"
    printf 'FAIL %s [%s]: bin_dir %s is configured but does not exist — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  # `fail_infra` inside count_executables would exit only the command
  # substitution, so its status is carried out explicitly — the same subshell
  # trap that made partial node discovery pass silently.
  executables="$(count_executables "${node}" "${bin_dir}")" || exec_status=$?
  [[ "${exec_status}" -eq 0 ]] ||
    fail_infra "could not list ${bin_dir} on ${node} (the directory exists but talosctl ls failed)"

  if [[ "${executables}" -eq 0 ]]; then
    printf 'FAIL %s [%s]: bin_dir %s holds no executable — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  printf 'OK   %s [%s]: bin_dir %s holds %s executable(s)\n' \
    "${node}" "${file}" "${bin_dir}" "${executables}"
  return 0
}

# One full sweep of the fleet currently in `nodes`. Kept as a function so the
# discovery path can run it again on a changed inventory; it is deliberately NOT
# invoked in a command substitution, because `fail_infra` inside one would exit
# only the subshell and let a fleet that was never checked report a verdict --
# the same trap the discovery path documents above.
run_check_pass() {
  failures=0
  for node in "${nodes[@]}"; do
    # Every containerd instance present on the node is evaluated INDEPENDENTLY.
    # Accepting the first config that declares a bin_dir would let a wired-up CRI
    # containerd mask an unprotected system containerd (or the reverse) — and the
    # two pull different images: CRI pulls workload images, the system instance
    # pulls Talos' own. A verifier on one leaves the other open, which is the
    # whole point of checking both.
    configs_present=0
    node_failures=0
    for config_file in "${config_files[@]}"; do
      case "$(path_probe "${node}" "${config_file}")" in
        present) ;;
        absent)
          # A confirmed not-found normally proves the node answered — but the same
          # phrase can come from a client-side fault (a missing talosconfig, say),
          # which would otherwise read as "this node does not ship that config".
          # The reachability probe is what tells those two apart.
          node_reachable "${node}" ||
            fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
          continue
          ;;
        *)
          # Both "the node is gone" and "the node answered but this one probe
          # failed" arrive here, and they deserve different diagnoses: the first
          # is a fleet-wide fault, the second is specific to one containerd.
          # Neither is a verdict, so both stop the run either way.
          node_reachable "${node}" ||
            fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
          fail_infra "could not determine whether ${config_file} exists on ${node} (talosctl ls failed for a reason other than the file being absent) — refusing to report a node whose containerd was never inspected"
          ;;
      esac
      configs_present=$((configs_present + 1))
      check_config "${node}" "${config_file}" || node_failures=$((node_failures + 1))
    done

    # No containerd configuration at all is not a clean node — it means this check
    # looked at nothing and would otherwise pass the node by default.
    [[ "${configs_present}" -gt 0 ]] ||
      fail_infra "no containerd configuration found on ${node} (looked in ${config_files[*]})"

    [[ "${node_failures}" -eq 0 ]] || failures=$((failures + 1))
  done
}

# Rebuilds `nodes` from a UID/address identity list.
nodes_from_identities() {
  nodes=()
  while IFS=$'\t' read -r _uid identity_address; do
    [[ -n "${identity_address}" ]] || continue
    nodes+=("${identity_address}")
  done <<<"$1"
}

# The inventory is a snapshot taken BEFORE a serial pass, and Cluster Autoscaler
# can add or replace workers while that pass runs. A node that joins mid-run --
# or a replacement reusing a retired address -- would then go completely
# uninspected while every node in the stale snapshot reported healthy: a green
# verdict for a fleet that was never fully enumerated, which is the same
# fail-open shape this script exists to detect.
#
# So the discovery path re-reads the inventory AFTER the checks and only reports
# when the fleet it just checked is still the fleet that exists. A change is
# ordinary autoscaling rather than a fault, so it costs a re-run, not a failure;
# only an inventory that will not settle within the bound is an error, which
# keeps a continuously-scaling cluster from either flapping or looping forever.
# `sync_talos_registry_auth` in scripts/refresh-flux-ghcr-auth.sh converges the
# same way and for the same reason.
#
# An explicitly requested fleet is NOT re-read: the caller named those nodes, so
# a cluster changing under them does not change what was asked for.
convergence_attempts="${IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS:-3}"
case "${convergence_attempts}" in
  '' | *[!0-9]*) fail_infra "IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS must be a positive integer, got '${convergence_attempts}'" ;;
esac
# Digits alone are not enough. Bash arithmetic applies BASE-PREFIX rules to a
# leading zero, so a documented, digit-only override is silently misread:
# measured, `[[ 08 -ge 1 ]]` errors with "value too great for base" and rejects a
# positive integer outright, while 010 evaluates to EIGHT and quietly runs a
# different number of convergence attempts than the operator asked for. `10#`
# pins base 10; the empty value that would make it a syntax error is already out.
convergence_attempts="$((10#${convergence_attempts}))"
[[ "${convergence_attempts}" -ge 1 ]] ||
  fail_infra 'IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS must be at least 1'

if [[ "${nodes_requested}" -eq 1 ]]; then
  run_check_pass
else
  attempt=0
  while :; do
    attempt=$((attempt + 1))
    run_check_pass

    inventory_after="$(discover_nodes)" ||
      fail_infra 'could not re-read the node inventory after the checks — refusing to report a fleet that may have changed underneath the pass'

    # Compare the identity SET, not the stream. `kubectl get nodes` gives no
    # ordering guarantee, so a plain string compare would call a re-ordered but
    # otherwise identical fleet "changed", re-run the whole pass, and can burn
    # the attempt bound into an exit 2 for a cluster that never moved.
    [[ "$(printf '%s\n' "${inventory_after}" | LC_ALL=C sort)" != "$(printf '%s\n' "${discovered_identities}" | LC_ALL=C sort)" ]] || break

    [[ "${attempt}" -lt "${convergence_attempts}" ]] ||
      fail_infra "the node inventory changed during each of ${convergence_attempts} attempt(s) — refusing to report a fleet that never held still long enough to be checked"

    printf '\nThe node set changed while it was being checked (a node joined, left, or was replaced). Re-running over the current fleet — attempt %s of %s.\n' \
      "$((attempt + 1))" "${convergence_attempts}"
    discovered_identities="${inventory_after}"
    nodes_from_identities "${discovered_identities}"
    [[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check after the inventory changed'
  done
fi

if [[ "${failures}" -gt 0 ]]; then
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are not being evaluated on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3101 (installing the verifier).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
