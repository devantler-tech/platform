# Bare local initialization guard

Run `go run ./scripts/guard-bash-local-initialization scripts .github/scripts`.
The guard parses Bash source without running it and reports function-local
variables read before an explicit assignment while `nounset` is enabled.
An empty declaration such as `local queue` leaves the variable unset on modern
Bash, even though Bash 3.2 initializes it to an empty string.

The check follows textual order within each function. It recognizes explicit
assignments, default assignments, arithmetic assignments, loop variables, and
literal destinations of `read`, `mapfile`, and `printf -v`. Safe default
expansions are allowed. Function scopes are checked independently.
Declaration initializers expand before any variable in that declaration is
assigned. A global declaration does not initialize a previously declared local.

This is a bounded lint, not complete control-flow analysis. Conditional writes
are considered assignments; it does not prove that a branch runs, resolve
dynamic variable names or namerefs, or model subshell propagation. Global and
array declarations are outside its scope. The inherited `nounset` state comes
from the script's static top-level `set` options; function-local `set` options
are then applied in textual order. ShellCheck and runtime tests remain required.

Exit 0 means all examined shell files passed, exit 1 reports findings, and
exit 2 means a file could not be read or parsed, or no shell files were found.
Every successful scan reports its examined file count.
