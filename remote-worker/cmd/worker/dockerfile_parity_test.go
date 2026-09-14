package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Nothing in CI builds either leaf Dockerfile -- build.yaml's matrix is the harness, the OCP sandbox
// and the echo target, and it only fires on push to main; ci.yml's remote-worker job is the Go build.
// hadolint parses these files but does not resolve packages. So the one mismatch that actually bit us
// -- `probed` naming a tool the image does not install, which shipped as `caps=[bash base64 file]` on
// a real VM -- had no guard, and neither did the two Dockerfiles' agreement with each other.
//
// This is that guard, and it is static on purpose: it relates the Go list to the image recipes without
// a daemon, a network or a build, so it runs in the `go test -race ./...` step that already exists.
// What it CANNOT do is prove a package still resolves in a future UBI 9 minor; only building the image
// does that. deploy/knative/verify-sandbox-inventory.sh is the in-image counterpart for the OCP
// sandbox image, and is the pattern to follow if these images ever get published to GHCR.
//
// Everything here matches against INSTRUCTIONS, never against the raw file. The first version of this
// test used strings.Contains over the whole body, which meant four of the six tokens were satisfied by
// the surrounding comment prose -- "the worker runs `bash -c`", "git and python3 are here because...",
// and `file` inside the word "Dockerfile". Deleting `git python3` from both install lines passed. A
// guard that a comment can satisfy is worse than no guard, because it reads as covered.

// source says what puts a probed tool on PATH and, critically, WHERE that has to appear. A package has
// to be a field of the microdnf install list; a vendored binary has to appear in a RUN instruction.
// Neither is satisfiable by a comment.
//
// The indirection through the token is the point: `base64` comes from coreutils-single and `rg` from a
// tarball, so a test that merely looked for the tool's own name would pass while the image stayed
// broken.
type source struct {
	pkg      string // must appear as a package in the runtime stage's microdnf install list
	vendored string // must appear in some RUN instruction (comments stripped)
}

var provides = map[string]source{
	"bash":    {pkg: "bash"},
	"base64":  {pkg: "coreutils-single"},
	"file":    {pkg: "file"},
	"git":     {pkg: "git"},
	"python3": {pkg: "python3"},
	"rg":      {vendored: "ripgrep-${RG_VERSION}-${target}.tar.gz"},
}

// knownGap is for a probed tool deliberately left out of the images. It must stay EMPTY unless the
// omission is also written into the Dockerfile comments and the PR that adds it, because an entry
// here means the agent's tools built on that binary fail at runtime in every leaf sandbox.
var knownGap = map[string]string{}

var (
	installLine = regexp.MustCompile(`(?m)^RUN microdnf install -y --nodocs (.+?)\s*\\?$`)
	// The /workspace block, captured as flags + path so the two files can be compared on owner,
	// group and mode rather than on whether the words appear somewhere.
	workspaceInstall = regexp.MustCompile(`(?m)^RUN install -d ((?:-[ogm] \S+ +)+)/workspace\s*$`)
	workdirLine      = regexp.MustCompile(`(?m)^WORKDIR /workspace\s*$`)
	commentLine      = regexp.MustCompile(`(?m)^\s*#.*$`)
)

func dockerfiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"Dockerfile", "Dockerfile.runtime"} {
		// The test binary runs in cmd/worker; both Dockerfiles sit at the module root.
		path := filepath.Join("..", "..", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out[name] = string(b)
	}
	return out
}

// code returns the Dockerfile with every comment line removed, so no assertion below can be satisfied
// by prose. This is the whole reason the earlier revision of this test was hollow.
func code(body string) string {
	return commentLine.ReplaceAllString(body, "")
}

// runtimePackages returns the package list from the runtime stage's microdnf install. The last match
// is the runtime stage's: the rg fetch stage installs only tar/gzip.
func runtimePackages(t *testing.T, name, body string) []string {
	t.Helper()
	m := installLine.FindAllStringSubmatch(code(body), -1)
	if len(m) == 0 {
		t.Fatalf("%s: no `RUN microdnf install` line found -- this test's regex has gone stale, "+
			"which would make it pass vacuously", name)
	}
	return strings.Fields(m[len(m)-1][1])
}

// Every tool the worker advertises must be installed by both images that can serve as its sandbox.
func TestDockerfilesProvideEveryProbedCapability(t *testing.T) {
	for name, body := range dockerfiles(t) {
		pkgs := runtimePackages(t, name, body)
		body := code(body)
		for _, tool := range probed {
			if why, ok := knownGap[tool]; ok {
				t.Logf("%s: %q is a declared gap (%s)", name, tool, why)
				continue
			}
			src, ok := provides[tool]
			if !ok {
				t.Errorf("%s: probed tool %q has no entry in the `provides` map. Add the package or "+
					"vendoring step that puts it on PATH, or record it in knownGap and say so in the "+
					"Dockerfile -- an unmapped tool is exactly the drift this test exists to catch.",
					name, tool)
				continue
			}
			switch {
			case src.pkg != "":
				// Exact field match against the install list, not a substring of the file.
				if !slices.Contains(pkgs, src.pkg) {
					t.Errorf("%s does not install %q: `probed` advertises it, but %q is not in the "+
						"runtime stage's microdnf install list (%v). The worker would report caps "+
						"without it and the agent's tools built on it would fail with exit 127 "+
						"inside a healthy sandbox.", name, tool, src.pkg, pkgs)
				}
			case src.vendored != "":
				if !strings.Contains(body, src.vendored) {
					t.Errorf("%s does not vendor %q: no RUN instruction mentions %q. `probed` "+
						"advertises the tool, so the agent's Grep and Find tools would fail with "+
						"exit 127 inside a healthy sandbox.", name, tool, src.vendored)
				}
			default:
				t.Errorf("%s: `provides[%q]` sets neither pkg nor vendored, so it asserts nothing",
					name, tool)
			}
		}
	}
}

// The two files are related only by a "see the same block in ./Dockerfile" comment, so nothing but a
// test keeps their package sets in step. Drift here means one sandbox image silently differs from the
// other depending on which build path an operator happened to use.
func TestBothDockerfilesInstallTheSamePackages(t *testing.T) {
	files := dockerfiles(t)
	a := runtimePackages(t, "Dockerfile", files["Dockerfile"])
	b := runtimePackages(t, "Dockerfile.runtime", files["Dockerfile.runtime"])
	// Sets, not sequences: reordering one list is a no-op for the image, and comparing the joined
	// strings would report "different package sets" while printing two lists that look equivalent.
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Errorf("the two leaf Dockerfiles install different package sets, so the image an operator "+
			"gets depends on which build path they used:\n  Dockerfile:         %v\n  Dockerfile.runtime: %v", a, b)
	}
}

// /workspace was this PR's primary find and the more confusing of the two failures: the exec dies on
// `cd /workspace` before running anything the caller asked for, so a sandbox that is attached, healthy
// and reachable returns a tool error. It was also the one thing this file did not cover -- both the
// `install -d` and the `WORKDIR` could be dropped from either file, or drift apart, with every other
// test here still green.
func TestBothDockerfilesCreateWorkspaceIdentically(t *testing.T) {
	flags := map[string]string{}
	for name, body := range dockerfiles(t) {
		body := code(body)
		m := workspaceInstall.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s: no `RUN install -d ... /workspace` instruction. Without the directory the "+
				"worker's `bash -c` dies on `cd /workspace: No such file or directory` and every "+
				"tool call fails inside a sandbox that looks healthy.", name)
			continue
		}
		flags[name] = strings.Join(strings.Fields(m[1]), " ")

		// runner.go's "every command is self-contained as `cd 'cwd' && ...`" is the only thing making
		// the process cwd irrelevant today. WORKDIR is what keeps a bare command, or an operator's
		// `podman exec`, from landing in / -- a second round of the same bug.
		if !workdirLine.MatchString(body) {
			t.Errorf("%s: no `WORKDIR /workspace`. Both sandbox images that came before this one set "+
				"it, and without it any exec path that sends a bare command lands in /.", name)
		}
	}
	if a, b := flags["Dockerfile"], flags["Dockerfile.runtime"]; a != "" && b != "" && a != b {
		t.Errorf("the two leaf Dockerfiles create /workspace with different owner/group/mode, so an "+
			"agent's ability to write in its own workspace depends on which build path an operator "+
			"used:\n  Dockerfile:         install -d %s /workspace\n  Dockerfile.runtime: install -d %s /workspace", a, b)
	}
}

// A vendored binary is only safe if the build fails closed on a bad download, so the digest and the
// verification step must both survive edits to these files.
func TestVendoredRipgrepIsPinnedAndChecksummed(t *testing.T) {
	for name, body := range dockerfiles(t) {
		body := code(body)
		for _, want := range []string{
			"ARG RG_VERSION=",             // pinned release, not "latest"
			"ARG RG_SHA256_X86_64=",       // both arches carry a digest
			"ARG RG_SHA256_AARCH64=",      //
			"sha256sum -c /tmp/rg.sha256", // and the digest is actually enforced
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: vendored ripgrep is missing %q. Without it the build would accept "+
					"whatever bytes the network returned for a binary that then executes "+
					"model-authored commands.", name, want)
			}
		}
	}
}
