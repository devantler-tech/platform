package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	fetchAttempts = 3
	fetchTimeout  = time.Minute
)

// ksailRepository is the public source the pinned KSail release is built from.
// It and fetchBackoff are variables so tests can stand in a local source and
// avoid waiting.
var (
	ksailRepository = "https://github.com/devantler-tech/ksail.git"
	fetchBackoff    = 5 * time.Second
)

var (
	releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	gitObjectID    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// foldInputs are the parts of the KSail source the mirrored fold depends on,
// with the git object type each must have. A tree's object ID covers every
// file below it, so equal IDs mean byte-identical content.
var foldInputs = []struct{ path, kind string }{
	{"pkg/fsutil/configmanager", "tree"},
	{"pkg/fsutil/generator/talos", "tree"},
	{"pkg/apis", "tree"},
	{"charts", "tree"},
	{"go.mod", "blob"},
	{"go.sum", "blob"},
}

// auditedFoldInputs records every set of fold inputs whose fold was reviewed
// against this mirror. A release passes when all its inputs equal one set.
// Add a set only with the source audit README.md describes.
var auditedFoldInputs = []struct {
	audit string
	ids   map[string]string
}{
	{"7.193.6, 7.193.8 and 7.194.0", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "a23c242005321f3a03aa39ac026122ac0cabab4d",
		"charts":                     "1ddd64ee9b29fcd14402838ec9bba7795edd0fa9",
		"go.mod":                     "a77ef7b4247c1735ba84705c6570427a3c681ec1",
		"go.sum":                     "12ab4ed14cc66ea4f526188dd1c4dbc3d30e74a0",
	}},
	{"7.194.3 and 7.194.4", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "e7d6122e499761d5a12948422176f33c2f1a853a",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.5", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.7 and 7.194.8", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.9, 7.194.10, 7.195.0 and 7.195.1", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.197.1", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "e7265f2eee822033c9047eb50e7720872756aa03",
		"go.sum":                     "3e87842cfa29b03933d8a57c23204b340cf3c2b7",
	}},
	{"7.202.3", map[string]string{
		"pkg/fsutil/configmanager":   "ac334b6ba5b7e474bfa88e13486a1ab0d1d6feea",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "c4ed7098f7dce6dca0c65649cd8ab3aaeccdf6f4",
		"go.mod":                     "3ef4674d6c9d1507ec198883c78712ce74819866",
		"go.sum":                     "c780001120d55f3eb85838940f5f149e62202fab",
	}},
	{"7.202.28", map[string]string{
		"pkg/fsutil/configmanager":   "8c46721a9c9703046eb099dd29ccb693dc166d42",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "a3fec71c3c9ba1d8ecf78fe9b95943f32e5499ab",
		"go.mod":                     "aa4556d3ce50ca3a2f7397a5f0125d302bb9431f",
		"go.sum":                     "5c4e0d2343d48b4844bbbe1fd7e706b0e767439a",
	}},
	{"7.202.56", map[string]string{
		"pkg/fsutil/configmanager":   "88779a5a93cc49f116d4f5eda8389becc14d1cd8",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "b1d9e18582954ce6a9851a64bdd1c7344d36067a",
		"go.sum":                     "b11643f42c4e3950f2513b6370c4ede1a99f3a0e",
	}},
	{"7.202.59", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "b1d9e18582954ce6a9851a64bdd1c7344d36067a",
		"go.sum":                     "b11643f42c4e3950f2513b6370c4ede1a99f3a0e",
	}},
	{"7.202.66", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "809e056f5c9f7baebf059f6fb93be1a58c3c28ab",
		"go.sum":                     "ab72e244adaafb76a0847ddead8c60cd348ed5b7",
	}},
	{"7.202.68", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "2eb7b43e87f0b9cc4639e3bad2745a3e39ccad55",
		"go.sum":                     "7256567572094accdc4d69153a51769075a59c4f",
	}},
	{"7.202.73", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "c77ae4ac195816a5248bc84d321fe144c439d2b5",
		"go.sum":                     "df772f0a8d79381717fa545b4737a6d9e2b1738f",
	}},
	{"7.202.76", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "a5e740297fe5146bd5c6cdb7be384e30086cbafe",
		"go.sum":                     "d9fd4665d9a81a144e97620e87dee117ef72cd8a",
	}},
	{"7.202.80", map[string]string{
		"pkg/fsutil/configmanager":   "0a1e097499ebe1b1a244470eb5d219ac20d0d98a",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "a644feb9d45a26df44752dc0ba5176c8169ced27",
		"go.sum":                     "145fd747ed18247f76d278876179c5d43e10b782",
	}},
	{"7.203.0", map[string]string{
		"pkg/fsutil/configmanager":   "34ec5422a0f070dc9337a504ac7867a2cd9e929f",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "eb9aa3882541313597910ff597f3252386989811",
		"go.sum":                     "17c34155da2d2e5c5d008d8547a8a24b652f2f15",
	}},
	{"7.203.3", map[string]string{
		"pkg/fsutil/configmanager":   "34ec5422a0f070dc9337a504ac7867a2cd9e929f",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "ce9b68318e3a936d2b6ea21341e47f7f173ac4de",
		"go.sum":                     "291f4d311949e2831f348d4fbb6c5ba63583a18f",
	}},
	{"7.203.7", map[string]string{
		"pkg/fsutil/configmanager":   "34ec5422a0f070dc9337a504ac7867a2cd9e929f",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "af73cc01e178b6fb64d3318a8f1098f8d1e27bbd",
		"go.sum":                     "d998e7b389ddd0efa46019795bee46f66c54a1d0",
	}},
	{"7.203.11 and 7.203.13", map[string]string{
		"pkg/fsutil/configmanager":   "34ec5422a0f070dc9337a504ac7867a2cd9e929f",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "d3b1dbfdf762a53478e1135f32a929a15542122a",
		"go.sum":                     "647e3a4ead4a80bce18f079b2e58e05218f3b06f",
	}},
	{"7.203.14", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "d3b1dbfdf762a53478e1135f32a929a15542122a",
		"go.sum":                     "647e3a4ead4a80bce18f079b2e58e05218f3b06f",
	}},
	{"7.203.16", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "c9e6ad1ab080aaad97ec14fc747c25281a628f5b",
		"go.sum":                     "974fab9016031677ec1df786e9a35b6e062a8936",
	}},
	{"7.203.17", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "e843176075da60aca6a89143b23917e8ccfd4c98",
		"go.sum":                     "4d0e4a20f90b41c0da5d9a000ed730316afa0bdb",
	}},
	{"7.203.19", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "9e15dde619ade90f9665151c95f95957658e795a",
		"go.sum":                     "ab216f3b6420dd96a618f1b4af0b4141d67c367f",
	}},
	{"7.203.20", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "67add350e5cbe92ec7dceb3fd8ffcf0353b67261",
		"go.sum":                     "cce45d00dec323f10de4a76eb92dc8003e29857b",
	}},
	{"7.203.21", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "cc8d8c1eaee93942a0b4515bd1e3a053a10e0293",
		"go.sum":                     "ac5eaca71439011e25e49382c261eecd50b456af",
	}},
	{"7.203.34", map[string]string{
		"pkg/fsutil/configmanager":   "76455759a264c6a10ac3ceab608df912efb33307",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "d50e9dc79a9b9d0cd096ea449344a69d780b27e3",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "3670c2b6f18ddd14099b1d166cab6a12ed578229",
		"go.sum":                     "ce455b1ab6a93f3f1fe908e88c41ca86b2391991",
	}},
	{"7.204.30", map[string]string{
		"pkg/fsutil/configmanager":   "cf718ca52bd537acbb61209dd34c5d9259c5a5f8",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "4ad0d5e41cd15528bf5026096c0b15ee8e9ae157",
		"charts":                     "88e653c564b04a1e512dc6e37adbdaf9f61534a2",
		"go.mod":                     "b28f6dcf842eedd68b718ca07e3b7cdac21e9f62",
		"go.sum":                     "942be4893f34ab083016927eeed4d018f0a5acac",
	}},
	{"7.205.1", map[string]string{
		"pkg/fsutil/configmanager":   "cf718ca52bd537acbb61209dd34c5d9259c5a5f8",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "4ad0d5e41cd15528bf5026096c0b15ee8e9ae157",
		"charts":                     "88e653c564b04a1e512dc6e37adbdaf9f61534a2",
		"go.mod":                     "acd77b48dd689dc0c107d073e5a6e589517eba9c",
		"go.sum":                     "62c77b34dc8e9643f77bd73e5ce951aa9520bf59",
	}},
	{"7.205.3", map[string]string{
		"pkg/fsutil/configmanager":   "cf718ca52bd537acbb61209dd34c5d9259c5a5f8",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "4ad0d5e41cd15528bf5026096c0b15ee8e9ae157",
		"charts":                     "88e653c564b04a1e512dc6e37adbdaf9f61534a2",
		"go.mod":                     "4fcbf207e611621b91cc25a672a6416dfa8a20cc",
		"go.sum":                     "c527402f86a6be7fa984a3580f149281e1d01ddc",
	}},
}

// verifyFoldInputs passes only when every fold input of the pinned release was
// read and all of them equal one audited set. It returns the releases that set
// was audited at. A failed or incomplete read is an error, never a pass.
func verifyFoldInputs(version string, resolve func(version string) (map[string]string, error)) (string, error) {
	got, err := resolve(version)
	if err != nil {
		return "", fmt.Errorf("KSail %s fold inputs could not be read, so the fold is unverified: %w", version, err)
	}
	for _, input := range foldInputs {
		if !gitObjectID.MatchString(got[input.path]) {
			return "", fmt.Errorf("KSail %s fold input %s was not read, so the fold is unverified", version, input.path)
		}
	}
	// Report the difference against the nearest audited set, preferring the
	// most recent one, so the message names what a new audit has to cover.
	var nearest []string
	nearestAudit := ""
	for i := len(auditedFoldInputs) - 1; i >= 0; i-- {
		set := auditedFoldInputs[i]
		var changed []string
		for _, input := range foldInputs {
			if got[input.path] != set.ids[input.path] {
				changed = append(changed, input.path)
			}
		}
		if len(changed) == 0 {
			return set.audit, nil
		}
		if nearestAudit == "" || len(changed) < len(nearest) {
			nearest, nearestAudit = changed, set.audit
		}
	}
	if nearestAudit == "" {
		return "", fmt.Errorf("no audited KSail fold inputs are recorded")
	}
	sort.Strings(nearest)
	return "", fmt.Errorf("KSail %s changes fold input(s) %s since the set audited at %s; this requires a new source audit", version, strings.Join(nearest, ", "), nearestAudit)
}

// resolveFoldInputs reads the object ID of every fold input at the release tag
// of version from repository. It fetches the tagged commit's trees only.
func resolveFoldInputs(repository, version string) (map[string]string, error) {
	return resolveFoldInputsWithCleanup(repository, version, os.RemoveAll)
}

// foldInputsCheckoutPrefix names every disposable source checkout this helper
// creates in the temporary directory, so its own checkouts can be told apart
// from unrelated files other tools leave there.
const foldInputsCheckoutPrefix = "ksail-fold-inputs-"

// resolveFoldInputsWithCleanup binds source acceptance to removing its disposable
// checkout. The filesystem operation is supplied separately so removal failures
// can be exercised without depending on the process's filesystem privileges.
func resolveFoldInputsWithCleanup(repository, version string, remove func(string) error) (ids map[string]string, resultErr error) {
	if !releaseVersion.MatchString(version) {
		return nil, fmt.Errorf("%q is not a release version", version)
	}
	dir, err := os.MkdirTemp("", foldInputsCheckoutPrefix)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := remove(dir); err != nil {
			ids = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary source checkout: %w", err))
		}
	}()
	// Each command gets its own deadline, so one hung fetch cannot use up the
	// time the retries need.
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		// Ignore user, system and inherited configuration: a URL rewrite or a
		// GIT_DIR from the caller would make this read a different source than
		// the one named above, or write into the caller's repository.
		cmd.Env = append(withoutGitVariables(os.Environ()), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.Output()
		if err != nil {
			detail := ""
			if exit, ok := err.(*exec.ExitError); ok {
				detail = ": " + strings.TrimSpace(string(exit.Stderr))
			}
			return "", fmt.Errorf("git %s: %w%s", strings.Join(args, " "), err, detail)
		}
		return string(out), nil
	}
	if _, err := git("init", "--quiet"); err != nil {
		return nil, err
	}
	tag := "refs/tags/v" + version
	// One transient failure must not fail every pull request and deploy, so the
	// fetch is retried. An exhausted retry is still an error, never a pass.
	var fetchErrs []error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(fetchBackoff * time.Duration(attempt))
		}
		_, err := git("fetch", "--quiet", "--no-tags", "--depth=1", "--filter=blob:none", repository, tag)
		if err == nil {
			fetchErrs = nil
			break
		}
		fetchErrs = append(fetchErrs, err)
	}
	if len(fetchErrs) > 0 {
		return nil, errors.Join(fetchErrs...)
	}
	ids = map[string]string{}
	for _, input := range foldInputs {
		out, err := git("ls-tree", "FETCH_HEAD", "--", input.path)
		if err != nil {
			return nil, err
		}
		// One entry: "<mode> <type> <id>\t<path>".
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		meta, path, found := strings.Cut(lines[0], "\t")
		fields := strings.Fields(meta)
		if len(lines) != 1 || !found || path != input.path || len(fields) != 3 {
			return nil, fmt.Errorf("%s has no %s at %s", repository, input.path, tag)
		}
		if fields[1] != input.kind {
			return nil, fmt.Errorf("%s at %s is a %s, expected a %s", input.path, tag, fields[1], input.kind)
		}
		if !gitObjectID.MatchString(fields[2]) {
			return nil, fmt.Errorf("%s at %s has an unrecognised object ID %q", input.path, tag, fields[2])
		}
		ids[input.path] = fields[2]
	}
	return ids, nil
}

func withoutGitVariables(environment []string) []string {
	var kept []string
	for _, variable := range environment {
		if !strings.HasPrefix(variable, "GIT_") {
			kept = append(kept, variable)
		}
	}
	return kept
}
