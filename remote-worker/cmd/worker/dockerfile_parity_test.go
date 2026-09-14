package main

import (
	"os"
	"path/filepath"
	"regexp"
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
// provides maps each probed tool to the thing in the Dockerfile that puts it on PATH. The indirection
// is the point: `base64` comes from coreutils-single and `rg` from a vendored tarball, so a test that
// merely grepped for the tool's own name would pass while the image stayed broken.
var provides = map[string]string{
	"bash":    "bash",
	"base64":  "coreutils-single",
	"file":    "file",
	"git":     "git",
	"python3": "python3",
	"rg":      "ripgrep-${RG_VERSION}-${target}.tar.gz",
}

// knownGap is for a probed tool deliberately left out of the images. It must stay EMPTY unless the
// omission is also written into the Dockerfile comments and the PR that adds it, because an entry
// here means the agent's tools built on that binary fail at runtime in every leaf sandbox.
var knownGap = map[string]string{}

var installLine = regexp.MustCompile(`(?m)^RUN microdnf install -y --nodocs (.+?)\s*\\?$`)

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

// Every tool the worker advertises must be installed by both images that can serve as its sandbox.
func TestDockerfilesProvideEveryProbedCapability(t *testing.T) {
	for name, body := range dockerfiles(t) {
		for _, tool := range probed {
			if why, ok := knownGap[tool]; ok {
				t.Logf("%s: %q is a declared gap (%s)", name, tool, why)
				continue
			}
			token, ok := provides[tool]
			if !ok {
				t.Errorf("%s: probed tool %q has no entry in the `provides` map. Add the package or "+
					"vendoring step that puts it on PATH, or record it in knownGap and say so in the "+
					"Dockerfile -- an unmapped tool is exactly the drift this test exists to catch.",
					name, tool)
				continue
			}
			if !strings.Contains(body, token) {
				t.Errorf("%s does not install %q: `probed` advertises it as a capability, but %q "+
					"appears nowhere in the file. The worker would report caps without it and the "+
					"agent's tools built on it would fail with exit 127 inside a healthy sandbox.",
					name, tool, token)
			}
		}
	}
}

// The two files are related only by a "see the same block in ./Dockerfile" comment, so nothing but a
// test keeps their package sets in step. Drift here means one sandbox image silently differs from the
// other depending on which build path an operator happened to use.
func TestBothDockerfilesInstallTheSamePackages(t *testing.T) {
	files := dockerfiles(t)
	sets := map[string][]string{}
	for name, body := range files {
		m := installLine.FindAllStringSubmatch(body, -1)
		if len(m) == 0 {
			t.Fatalf("%s: no `RUN microdnf install` line found -- this test's regex has gone stale, "+
				"which would make it pass vacuously", name)
		}
		// The last match is the runtime stage's: the rg fetch stage installs only tar/gzip.
		pkgs := strings.Fields(m[len(m)-1][1])
		sets[name] = pkgs
	}
	a, b := sets["Dockerfile"], sets["Dockerfile.runtime"]
	if strings.Join(a, " ") != strings.Join(b, " ") {
		t.Errorf("the two leaf Dockerfiles install different package sets, so the image an operator "+
			"gets depends on which build path they used:\n  Dockerfile:         %v\n  Dockerfile.runtime: %v", a, b)
	}
}

// A vendored binary is only safe if the build fails closed on a bad download, so the digest and the
// verification step must both survive edits to these files.
func TestVendoredRipgrepIsPinnedAndChecksummed(t *testing.T) {
	for name, body := range dockerfiles(t) {
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
