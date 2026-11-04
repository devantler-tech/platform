# Native Go source analysis

OSV 2.3.8 embeds an analyzer that cannot read the repository's Go 1.27 code.
The adapter builds unmodified OSV 2.7.0 with the pinned Go 1.27.3 compiler and
checks its release checksum and binary metadata. It retains complete recursive
source scanning and native Go call analysis.

OSV 2.7 omits declared standard-library versions by default. The adapter
explicitly sets `ScanGoModVersion = true` for every tracked Go module through
private local configurations. The existing three Kyverno database-range
decisions remain limited to their original module and reviewed release.

Before every scan, the byte-bound old-compiler control is materialized outside
the checkout. The same native scanner must return its finding exit and report
called advisories under the real Go 1.24 toolchain. No control program executes.
An absent database result or failed analyzer refuses the full scan.

The scanner reads the same private Git checkout whose source and configuration
receipts are checked before and after it. Positive native extraction records
must account for every tracked module; every native finding and diagnostic is
retained. Run `bash scripts/osv-source-analysis.sh scan source --recursive .`
from a committed checkout for the standalone diagnostic.
