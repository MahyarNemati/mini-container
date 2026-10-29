package spec_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MahyarNemati/mini-container/internal/spec"
)

func valid() *spec.Spec {
	return &spec.Spec{
		ID:         "test-container",
		RootFS:     "/tmp/rootfs",
		Args:       []string{"/bin/sh"},
		Namespaces: spec.DefaultNamespaces(),
	}
}

func TestValidSpecPasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestIDMustBeSafeAsAPathComponent(t *testing.T) {
	// The id becomes a cgroup directory name and a filesystem path, so
	// anything that could escape either must be rejected.
	for _, id := range []string{"", "a/b", "../escape", "a b", "a;rm -rf /", "a$(x)", strings.Repeat("a", 65)} {
		s := valid()
		s.ID = id
		if err := s.Validate(); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
	for _, id := range []string{"a", "my-container_1", "ABC123"} {
		s := valid()
		s.ID = id
		if err := s.Validate(); err != nil {
			t.Errorf("id %q should be accepted: %v", id, err)
		}
	}
}

func TestArgsRequired(t *testing.T) {
	s := valid()
	s.Args = nil
	if err := s.Validate(); err == nil {
		t.Error("a spec with no command should be rejected")
	}
}

func TestExactlyOneRootSource(t *testing.T) {
	s := valid()
	s.RootFS, s.Image = "", ""
	if err := s.Validate(); err == nil {
		t.Error("neither rootfs nor image should be rejected")
	}

	s = valid()
	s.Image = "/tmp/image"
	if err := s.Validate(); err == nil {
		t.Error("both rootfs and image should be rejected")
	}
}

func TestMountNamespaceIsRequired(t *testing.T) {
	// Without a mount namespace, pivot_root would change the *host's* root,
	// which is a spectacular way to break a machine.
	s := valid()
	s.Namespaces.Mount = false
	err := s.Validate()
	if err == nil {
		t.Fatal("changing the root filesystem without a mount namespace must be rejected")
	}
	if !errors.Is(err, spec.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestMountDestinationsMustBeAbsoluteAndContained(t *testing.T) {
	for _, dest := range []string{"", "relative/path", "/ok/../../../etc", "/a/../b"} {
		s := valid()
		s.Mounts = []spec.Mount{{Source: "/tmp", Destination: dest}}
		if err := s.Validate(); err == nil {
			t.Errorf("destination %q should be rejected", dest)
		}
	}
	s := valid()
	s.Mounts = []spec.Mount{{Source: "/tmp", Destination: "/data"}}
	if err := s.Validate(); err != nil {
		t.Errorf("/data should be accepted: %v", err)
	}
}

func TestNegativeResourcesRejected(t *testing.T) {
	for _, mutate := range []func(*spec.Spec){
		func(s *spec.Spec) { s.Resources.MemoryBytes = -1 },
		func(s *spec.Spec) { s.Resources.CPUMillis = -1 },
		func(s *spec.Spec) { s.Resources.PidsMax = -1 },
	} {
		s := valid()
		mutate(s)
		if err := s.Validate(); err == nil {
			t.Error("a negative resource limit should be rejected")
		}
	}
}

func TestIOWeightRange(t *testing.T) {
	for _, w := range []int64{-1, 0 - 1, 10001, 99999} {
		s := valid()
		s.Resources.IOWeight = w
		if err := s.Validate(); err == nil {
			t.Errorf("io_weight %d should be rejected", w)
		}
	}
	// Zero means "unset", which is valid.
	s := valid()
	s.Resources.IOWeight = 0
	if err := s.Validate(); err != nil {
		t.Errorf("io_weight 0 means unset and should be accepted: %v", err)
	}
}

func TestDefaultNamespacesOmitsUserAndNetwork(t *testing.T) {
	ns := spec.DefaultNamespaces()
	if !ns.Mount || !ns.PID || !ns.UTS || !ns.IPC {
		t.Errorf("expected mount, pid, uts and ipc by default, got %+v", ns)
	}
	// Both need extra setup to be useful: a network namespace starts with
	// only loopback, and a user namespace has surprising interactions.
	if ns.Network || ns.User {
		t.Errorf("network and user namespaces should be opt-in, got %+v", ns)
	}
}

func TestDefaultEnvDoesNotInheritTheHost(t *testing.T) {
	env := spec.DefaultEnv("box")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=", "HOME=", "HOSTNAME=box"} {
		if !strings.Contains(joined, want) {
			t.Errorf("default env missing %q: %v", want, env)
		}
	}
	// A container that sees the host's environment leaks whatever secrets
	// are in it.
	if len(env) > 6 {
		t.Errorf("default env looks like it inherited the host: %v", env)
	}
}
