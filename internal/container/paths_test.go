package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mount destination comes from a spec file, so it is input like any other.
// These cases need no root and no namespaces, which is why they are here
// rather than in the integration suite.
func TestResolveInRootRejectsEscapes(t *testing.T) {
	root := t.TempDir()

	t.Run("inside the root", func(t *testing.T) {
		for _, dest := range []string{"/data", "/var/lib/thing", "/a/b/c", "/data/"} {
			got, err := resolveInRoot(root, dest)
			if err != nil {
				t.Fatalf("resolveInRoot(%q): %v", dest, err)
			}
			if !strings.HasPrefix(got, root+"/") {
				t.Errorf("resolveInRoot(%q) = %q, which is outside %q", dest, got, root)
			}
		}
	})

	t.Run("escapes are refused", func(t *testing.T) {
		// Cleaning these to a host path instead of refusing them would let a
		// spec write outside the container before the workload ever starts --
		// and leave nothing in the logs to notice.
		for _, dest := range []string{
			"/../etc",
			"/../../etc/passwd",
			"/data/../../..",
			"/a/../../b",
			// Even one that stays inside: a destination with a ".." is not a
			// path anyone meant to write, and cleaning it away is how the
			// previous version looked safe without being honest.
			"/a/b/../c",
		} {
			got, err := resolveInRoot(root, dest)
			if err == nil {
				t.Errorf("resolveInRoot(%q) = %q, want an error", dest, got)
			}
		}
	})

	t.Run("relative destinations are refused", func(t *testing.T) {
		// A relative destination has no meaning inside a container: there is
		// no cwd yet when the mounts are set up.
		for _, dest := range []string{"data", "./data", "", "../data"} {
			if _, err := resolveInRoot(root, dest); err == nil {
				t.Errorf("resolveInRoot(%q) succeeded, want an error", dest)
			}
		}
	})

	t.Run("the root itself", func(t *testing.T) {
		got, err := resolveInRoot(root, "/")
		if err != nil {
			t.Fatalf("resolveInRoot(%q, \"/\"): %v", root, err)
		}
		if got != filepath.Clean(root) {
			t.Errorf("resolveInRoot(root, \"/\") = %q, want %q", got, root)
		}
	})

	t.Run("a symlink out of the root is refused", func(t *testing.T) {
		// mount(2) follows symlinks, so a destination pointing at one that
		// leaves the rootfs would mount onto the host.
		if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveInRoot(root, "/escape"); err == nil {
			t.Errorf("resolveInRoot(%q) = %q, want an error", "/escape", got)
		}
		// One that stays inside is fine.
		if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(root, "inside")); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveInRoot(root, "/inside"); err != nil {
			t.Errorf("resolveInRoot(%q): %v", "/inside", err)
		}
	})

	t.Run("a trailing slash does not change the answer", func(t *testing.T) {
		with, err1 := resolveInRoot(root, "/data/")
		without, err2 := resolveInRoot(root, "/data")
		if err1 != nil || err2 != nil {
			t.Fatalf("errors: %v %v", err1, err2)
		}
		if with != without {
			t.Errorf("%q != %q", with, without)
		}
	})
}
