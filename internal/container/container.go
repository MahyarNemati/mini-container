// Package container runs a process in its own namespaces.
//
// # How a container is actually started
//
// There is no single syscall for "run this program in new namespaces". The
// sequence a runtime must perform is:
//
//  1. `clone` with the CLONE_NEW* flags, so the child starts in fresh
//     namespaces.
//  2. *Inside the child*, set up the environment: mount /proc, pivot into the
//     new root, set the hostname.
//  3. `exec` the workload, replacing the child.
//
// Step 2 is the problem in Go. `clone` with namespace flags cannot be called
// safely from a multi-threaded process -- and the Go runtime is always
// multi-threaded -- because CLONE_NEWNS and CLONE_NEWPID apply to the calling
// *thread*, and Go gives you no control over which thread runs your code or
// what the others are doing.
//
// The standard solution, which runc also uses, is to **re-execute the runtime
// binary**: the parent spawns /proc/self/exe with a sentinel argument and the
// namespace flags in SysProcAttr, so the kernel does the clone. The new
// process starts single-threaded in the new namespaces, does the setup, and
// execs the workload. That is why this package has an Init function that is
// only ever called in a child process.
package container

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/MahyarNemati/mini-container/internal/cgroups"
	"github.com/MahyarNemati/mini-container/internal/spec"
)

// InitArg is the sentinel that marks a re-executed child. A user cannot
// reasonably pass it by accident, and the child checks for it before doing
// anything destructive like pivot_root.
const InitArg = "--mini-container-init"

// Result describes a finished container.
type Result struct {
	ExitCode int
	// Signal is non-zero when the process was killed by one.
	Signal int
	// OOMKilled is read from the cgroup's memory.events, which is the only
	// reliable way to distinguish an OOM kill from an ordinary non-zero exit:
	// the process just sees SIGKILL either way.
	OOMKilled bool
	Duration  time.Duration
	Stats     cgroups.Stats
	// TimedOut is true when the container was killed by its own timeout.
	TimedOut bool
}

// Errors returned by this package.
var (
	ErrUnsupportedPlatform = errors.New("container: containers require Linux")
	ErrNotRoot             = errors.New("container: must run as root, or with a user namespace")
)

// Config bundles what Run needs beyond the spec.
type Config struct {
	Spec *spec.Spec
	// CgroupRoot defaults to /sys/fs/cgroup.
	CgroupRoot string
	// StateDir holds overlay layers and other per-container scratch.
	StateDir string
	Stdin    *os.File
	Stdout   *os.File
	Stderr   *os.File
}

func (c *Config) withDefaults() {
	if c.CgroupRoot == "" {
		c.CgroupRoot = cgroups.DefaultRoot
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/mini-container"
	}
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
}

// Preflight reports why a container cannot be started here, so the CLI can
// explain the problem instead of failing on an opaque syscall error.
func Preflight(cgroupRoot string) error {
	if err := platformCheck(); err != nil {
		return err
	}
	if cgroupRoot == "" {
		cgroupRoot = cgroups.DefaultRoot
	}
	if err := cgroups.Available(cgroupRoot); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w (euid %d)", ErrNotRoot, os.Geteuid())
	}
	return nil
}
