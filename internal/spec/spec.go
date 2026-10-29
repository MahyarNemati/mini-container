// Package spec describes what a container is in this implementation.
//
// Deliberately much smaller than the OCI runtime spec: that document is ~40
// pages and most of it exists to support things a teaching implementation
// should not pretend to handle (seccomp profiles, SELinux labels, device
// cgroups, hook lifecycles). This is the subset that actually isolates a
// process.
package spec

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Namespaces selects which namespaces to unshare.
//
// Each one hides a different global resource, and the interesting part is
// which are cheap and which have consequences:
//
//   - **Mount** is the one that makes a container a container: without it the
//     process still sees the host filesystem and nothing else matters.
//   - **PID** gives the process pid 1, which means it inherits the duty of
//     reaping orphans. Forget that and the container accumulates zombies.
//   - **UTS** is just the hostname. Nearly free, and what makes `hostname`
//     inside the container not lie.
//   - **IPC** hides System V shared memory and semaphores. Rarely noticed
//     until two containers collide on a well-known key.
//   - **Network** is the expensive one: a new netns starts with only a
//     loopback device, so anything that needs connectivity requires a veth
//     pair and routing set up from the host.
//   - **User** is the one that allows unprivileged containers, by mapping
//     root inside to an unprivileged uid outside. It is also the one with the
//     most surprising interactions, which is why it is off by default.
type Namespaces struct {
	Mount   bool `json:"mount"`
	PID     bool `json:"pid"`
	UTS     bool `json:"uts"`
	IPC     bool `json:"ipc"`
	Network bool `json:"network"`
	User    bool `json:"user"`
	Cgroup  bool `json:"cgroup"`
}

// DefaultNamespaces is everything except user and network, which both need
// extra setup to be useful.
func DefaultNamespaces() Namespaces {
	return Namespaces{Mount: true, PID: true, UTS: true, IPC: true, Cgroup: true}
}

// Resources are cgroup v2 limits. Zero means unlimited.
type Resources struct {
	// MemoryBytes maps to memory.max. Exceeding it means the OOM killer
	// targets a process inside the container rather than anything on the host,
	// which is the whole point of setting it.
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	// MemorySwapBytes maps to memory.swap.max. Worth setting to 0 alongside
	// a memory limit: otherwise a container that hits its limit degrades into
	// swap thrashing instead of failing, which is slower and harder to
	// diagnose than an OOM kill.
	MemorySwapBytes int64 `json:"memory_swap_bytes,omitempty"`
	// CPUMillis is thousandths of a core, mapped to cpu.max. 1500 means
	// 1.5 cores.
	CPUMillis int64 `json:"cpu_millis,omitempty"`
	// PidsMax maps to pids.max and is the cheapest protection against a fork
	// bomb. Without it, a process inside the container can exhaust the host's
	// pid space.
	PidsMax int64 `json:"pids_max,omitempty"`
	// IOWeight maps to io.weight (1-10000).
	IOWeight int64 `json:"io_weight,omitempty"`
}

// Mount is one additional mount inside the container.
type Mount struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Spec is a complete container definition.
type Spec struct {
	// ID identifies the container; used for the cgroup and state paths.
	ID string `json:"id"`
	// RootFS is the directory to pivot into. When Image is set, this is the
	// assembled overlay merge directory.
	RootFS string `json:"rootfs,omitempty"`
	// Image is an unpacked image directory used as the overlay lower layer.
	Image string `json:"image,omitempty"`
	// Args is the command and its arguments.
	Args []string `json:"args"`
	// Env is the environment. Deliberately not inherited: a container that
	// sees the host's environment leaks whatever secrets are in it.
	Env []string `json:"env,omitempty"`
	// Cwd is the working directory inside the container.
	Cwd        string     `json:"cwd,omitempty"`
	Hostname   string     `json:"hostname,omitempty"`
	Namespaces Namespaces `json:"namespaces"`
	Resources  Resources  `json:"resources"`
	Mounts     []Mount    `json:"mounts,omitempty"`
	// ReadOnlyRoot remounts the root filesystem read-only after setup.
	ReadOnlyRoot bool `json:"read_only_root,omitempty"`
	// Terminal attaches the host's stdio.
	Terminal bool `json:"terminal,omitempty"`
	// Timeout kills the container after this long. Zero means no limit.
	Timeout time.Duration `json:"timeout,omitempty"`
	// UIDMap and GIDMap configure a user namespace: the container's root maps
	// to this host id.
	HostUID int `json:"host_uid,omitempty"`
	HostGID int `json:"host_gid,omitempty"`
}

// ErrInvalid is returned when a spec is unusable.
var ErrInvalid = errors.New("spec: invalid")

// Validate checks the spec.
func (s *Spec) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalid)
	}
	// The id becomes a cgroup directory name and a path component, so reject
	// anything that could escape either.
	for _, c := range s.ID {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("%w: id may only contain letters, digits, '-' and '_'; found %q", ErrInvalid, c)
		}
	}
	if len(s.ID) > 64 {
		return fmt.Errorf("%w: id is longer than 64 characters", ErrInvalid)
	}
	if len(s.Args) == 0 {
		return fmt.Errorf("%w: args is required", ErrInvalid)
	}
	if s.RootFS == "" && s.Image == "" {
		return fmt.Errorf("%w: one of rootfs or image is required", ErrInvalid)
	}
	if s.RootFS != "" && s.Image != "" {
		return fmt.Errorf("%w: set rootfs or image, not both", ErrInvalid)
	}
	// A rootfs without a mount namespace would pivot the *host's* root, which
	// is a spectacular way to break a machine.
	if !s.Namespaces.Mount {
		return fmt.Errorf("%w: a mount namespace is required to change the root filesystem", ErrInvalid)
	}
	if s.Resources.CPUMillis < 0 || s.Resources.MemoryBytes < 0 || s.Resources.PidsMax < 0 {
		return fmt.Errorf("%w: resource limits must not be negative", ErrInvalid)
	}
	if w := s.Resources.IOWeight; w != 0 && (w < 1 || w > 10000) {
		return fmt.Errorf("%w: io_weight must be between 1 and 10000", ErrInvalid)
	}
	for i, m := range s.Mounts {
		if m.Destination == "" {
			return fmt.Errorf("%w: mounts[%d] has no destination", ErrInvalid, i)
		}
		if !strings.HasPrefix(m.Destination, "/") {
			return fmt.Errorf("%w: mounts[%d] destination must be absolute", ErrInvalid, i)
		}
		// A destination containing ".." could place a mount outside the
		// container root.
		if strings.Contains(m.Destination, "..") {
			return fmt.Errorf("%w: mounts[%d] destination must not contain '..'", ErrInvalid, i)
		}
	}
	return nil
}

// DefaultEnv is a minimal environment. The host's environment is deliberately
// not inherited.
func DefaultEnv(hostname string) []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=xterm",
		"HOSTNAME=" + hostname,
	}
}
