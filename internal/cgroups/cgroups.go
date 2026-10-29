// Package cgroups applies cgroup v2 resource limits.
//
// cgroup v2 only. v1 had a separate hierarchy per controller, which meant a
// container's limits lived in a dozen unrelated directories and "which cgroup
// is this process in" had a dozen answers. v2 has one unified tree, and every
// distribution worth targeting has defaulted to it for years.
//
// The whole interface is writing text to files, which is why this package is
// portable Go with an injectable root: the logic is testable against a
// temporary directory on any OS, and only the real mount point is Linux
// specific.
package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultRoot is where cgroup v2 is mounted on a modern Linux system.
const DefaultRoot = "/sys/fs/cgroup"

// cpuPeriod is the scheduling window cpu.max is expressed against. 100ms is
// the kernel default and there is no reason to change it: a shorter period
// means more scheduling overhead, a longer one means burstier throttling.
const cpuPeriod = 100000

// Errors returned by this package.
var (
	ErrNotSupported = errors.New("cgroups: cgroup v2 is not available")
	ErrNoController = errors.New("cgroups: required controller is not enabled")
)

// Limits are the resource limits to apply. Zero means "do not set".
type Limits struct {
	MemoryBytes     int64
	MemorySwapBytes int64
	// CPUMillis is thousandths of a core: 1500 is 1.5 cores.
	CPUMillis int64
	PidsMax   int64
	IOWeight  int64
}

// Group is one cgroup directory.
type Group struct {
	root string
	path string
}

// New creates a cgroup under root. The caller must have write access, which
// in practice means root or a delegated subtree.
func New(root, name string) (*Group, error) {
	if root == "" {
		root = DefaultRoot
	}
	// A name containing a separator would create a nested group or escape the
	// root entirely.
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return nil, fmt.Errorf("cgroups: invalid group name %q", name)
	}

	path := filepath.Join(root, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("cgroups: create %s: %w", path, err)
	}
	return &Group{root: root, path: path}, nil
}

// Path returns the cgroup directory.
func (g *Group) Path() string { return g.path }

// Apply writes the limits.
//
// Each controller is written independently and a missing one is reported
// rather than ignored: silently not applying a memory limit is how a
// container escapes the limit its operator believes is in place.
func (g *Group) Apply(l Limits) error {
	var errs []error

	if l.MemoryBytes > 0 {
		if err := g.write("memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			errs = append(errs, err)
		}
	}
	// Writing swap even when zero is deliberate: with a memory limit and
	// unlimited swap, a container that hits its limit thrashes instead of
	// being killed, which is slower and much harder to diagnose.
	if l.MemorySwapBytes >= 0 && l.MemoryBytes > 0 {
		if err := g.write("memory.swap.max", strconv.FormatInt(l.MemorySwapBytes, 10)); err != nil {
			// Swap accounting is not always compiled in, and failing the
			// whole container over it would be unhelpful.
			errs = append(errs, fmt.Errorf("%w (swap accounting may be disabled)", err))
		}
	}
	if l.CPUMillis > 0 {
		if err := g.write("cpu.max", CPUMax(l.CPUMillis)); err != nil {
			errs = append(errs, err)
		}
	}
	if l.PidsMax > 0 {
		if err := g.write("pids.max", strconv.FormatInt(l.PidsMax, 10)); err != nil {
			errs = append(errs, err)
		}
	}
	if l.IOWeight > 0 {
		if err := g.write("io.weight", strconv.FormatInt(l.IOWeight, 10)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CPUMax renders a cpu.max value for a millicore quota.
//
// Exported because the format is non-obvious and worth testing directly:
// "<quota> <period>" in microseconds, or "max <period>" for unlimited.
func CPUMax(millis int64) string {
	if millis <= 0 {
		return fmt.Sprintf("max %d", cpuPeriod)
	}
	quota := millis * cpuPeriod / 1000
	return fmt.Sprintf("%d %d", quota, cpuPeriod)
}

// AddProcess moves a pid into the cgroup.
//
// cgroup.procs takes one pid per write and moves the whole thread group. The
// pid must be written *before* the process execs the workload, or it runs
// briefly outside the limits -- long enough for a fork bomb to matter.
func (g *Group) AddProcess(pid int) error {
	return g.write("cgroup.procs", strconv.Itoa(pid))
}

// Stats are the counters worth reporting after a run.
type Stats struct {
	// MemoryCurrent is memory.current.
	MemoryCurrent int64
	// MemoryPeak is memory.peak, where the kernel provides it.
	MemoryPeak int64
	// OOMKills is the oom_kill counter from memory.events, which is the only
	// reliable way to tell an OOM kill from an ordinary non-zero exit.
	OOMKills int64
	// CPUUsage is the usage_usec field of cpu.stat.
	CPUUsage time.Duration
	// Throttled is nr_throttled from cpu.stat: non-zero means the CPU limit
	// was actually reached, which is the difference between "slow" and
	// "limited".
	Throttled int64
	Pids      int64
}

// Stats reads the current counters. Missing files are not an error: which
// files exist depends on which controllers are enabled.
func (g *Group) Stats() Stats {
	var s Stats
	s.MemoryCurrent = g.readInt("memory.current")
	s.MemoryPeak = g.readInt("memory.peak")
	s.Pids = g.readInt("pids.current")

	for k, v := range g.readKeyed("memory.events") {
		if k == "oom_kill" {
			s.OOMKills = v
		}
	}
	for k, v := range g.readKeyed("cpu.stat") {
		switch k {
		case "usage_usec":
			s.CPUUsage = time.Duration(v) * time.Microsecond
		case "nr_throttled":
			s.Throttled = v
		}
	}
	return s
}

// Delete removes the cgroup. It only succeeds once every process inside has
// exited, so a failure here usually means something is still running.
func (g *Group) Delete() error {
	// Retry briefly: the kernel needs a moment after the last process exits
	// before the directory becomes removable, and failing immediately leaves
	// an orphan cgroup behind on every run.
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Remove(g.path); err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("cgroups: remove %s: %w", g.path, err)
}

// write sets a control file.
//
// Deliberately opened *without* O_CREATE. A cgroup control file already exists
// whenever its controller is enabled, so creating one is never correct -- and
// os.WriteFile's O_CREATE would mask a missing controller by happily creating
// a regular file that the kernel ignores. The operator would then believe a
// limit was applied when it was not.
func (g *Group) write(file, value string) error {
	path := filepath.Join(g.path, file)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s does not exist (is the controller enabled in the parent's cgroup.subtree_control?)", ErrNoController, file)
		}
		return fmt.Errorf("cgroups: open %s: %w", file, err)
	}
	defer f.Close()

	if _, err := f.WriteString(value); err != nil {
		return fmt.Errorf("cgroups: write %s=%q: %w", file, value, err)
	}
	return nil
}

func (g *Group) readInt(file string) int64 {
	raw, err := os.ReadFile(filepath.Join(g.path, file))
	if err != nil {
		return 0
	}
	// "max" is a valid value and is not a number.
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// readKeyed parses the "key value" per line format used by cpu.stat and
// memory.events.
func (g *Group) readKeyed(file string) map[string]int64 {
	out := map[string]int64{}
	raw, err := os.ReadFile(filepath.Join(g.path, file))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// Available reports whether cgroup v2 is mounted at root.
func Available(root string) error {
	if root == "" {
		root = DefaultRoot
	}
	// cgroup.controllers exists only in a v2 hierarchy, which makes it the
	// cheapest and most reliable v1-versus-v2 test.
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return fmt.Errorf("%w: no cgroup.controllers at %s", ErrNotSupported, root)
	}
	return nil
}

// Controllers lists the controllers available at root.
func Controllers(root string) ([]string, error) {
	if root == "" {
		root = DefaultRoot
	}
	raw, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotSupported, err)
	}
	return strings.Fields(string(raw)), nil
}

// EnableControllers writes to cgroup.subtree_control so child groups can use
// the named controllers.
//
// This is the step people miss. A controller being listed in
// cgroup.controllers means the *parent* has it; a child cgroup cannot use it
// until the parent delegates it through subtree_control. Without this,
// memory.max simply does not exist in the new group and the limit silently
// does nothing.
func EnableControllers(root string, names ...string) error {
	if root == "" {
		root = DefaultRoot
	}
	available, err := Controllers(root)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range available {
		have[c] = true
	}

	var enable []string
	var missing []string
	for _, n := range names {
		if have[n] {
			enable = append(enable, "+"+n)
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s not available at %s (have: %s)",
			ErrNoController, strings.Join(missing, ", "), root, strings.Join(available, " "))
	}
	if len(enable) == 0 {
		return nil
	}

	path := filepath.Join(root, "cgroup.subtree_control")
	if err := os.WriteFile(path, []byte(strings.Join(enable, " ")), 0o644); err != nil {
		return fmt.Errorf("cgroups: enable controllers: %w", err)
	}
	return nil
}
