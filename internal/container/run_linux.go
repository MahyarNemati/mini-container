//go:build linux

package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/MahyarNemati/mini-container/internal/cgroups"
	"github.com/MahyarNemati/mini-container/internal/overlay"
	"github.com/MahyarNemati/mini-container/internal/spec"
)

func platformCheck() error { return nil }

// cloneFlags maps the spec's namespace selection onto CLONE_NEW* flags.
func cloneFlags(ns spec.Namespaces) uintptr {
	var flags uintptr
	if ns.Mount {
		flags |= syscall.CLONE_NEWNS
	}
	if ns.PID {
		flags |= syscall.CLONE_NEWPID
	}
	if ns.UTS {
		flags |= syscall.CLONE_NEWUTS
	}
	if ns.IPC {
		flags |= syscall.CLONE_NEWIPC
	}
	if ns.Network {
		flags |= syscall.CLONE_NEWNET
	}
	if ns.User {
		flags |= syscall.CLONE_NEWUSER
	}
	if ns.Cgroup {
		flags |= unixCloneNewCgroup
	}
	return flags
}

// CLONE_NEWCGROUP is not in syscall, so define it rather than import a
// dependency for one constant.
const unixCloneNewCgroup = 0x02000000

// Run starts the container and waits for it to exit.
//
// This is the parent half. It never touches namespaces itself: it configures
// SysProcAttr and lets the kernel clone, because unsharing a mount or pid
// namespace from a multi-threaded Go process is not safe.
func Run(cfg Config) (*Result, error) {
	cfg.withDefaults()
	s := cfg.Spec
	if err := s.Validate(); err != nil {
		return nil, err
	}

	stateDir := filepath.Join(cfg.StateDir, s.ID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("container: create state dir: %w", err)
	}
	defer os.RemoveAll(stateDir)

	// --- root filesystem ---
	rootfs := s.RootFS
	var layers *overlay.Layout
	if s.Image != "" {
		if err := overlay.Supported(); err != nil {
			return nil, err
		}
		var err error
		layers, err = overlay.Prepare(stateDir, s.Image)
		if err != nil {
			return nil, err
		}
		if err := layers.Mount(); err != nil {
			return nil, err
		}
		defer func() {
			if err := layers.Unmount(); err != nil {
				fmt.Fprintf(cfg.Stderr, "warning: %v\n", err)
			}
		}()
		rootfs = layers.Merged
	}

	absRoot, err := filepath.Abs(rootfs)
	if err != nil {
		return nil, fmt.Errorf("container: resolve rootfs: %w", err)
	}
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("container: rootfs %s is not a directory", absRoot)
	}

	// --- cgroup ---
	// Controllers must be delegated by the parent before a child group can
	// use them; without this memory.max simply does not exist and the limit
	// silently does nothing.
	if err := cgroups.EnableControllers(cfg.CgroupRoot, "memory", "cpu", "pids"); err != nil {
		fmt.Fprintf(cfg.Stderr, "warning: %v\n", err)
	}
	group, err := cgroups.New(cfg.CgroupRoot, "minic-"+s.ID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := group.Delete(); err != nil {
			fmt.Fprintf(cfg.Stderr, "warning: %v\n", err)
		}
	}()
	if err := group.Apply(cgroups.Limits{
		MemoryBytes:     s.Resources.MemoryBytes,
		MemorySwapBytes: s.Resources.MemorySwapBytes,
		CPUMillis:       s.Resources.CPUMillis,
		PidsMax:         s.Resources.PidsMax,
		IOWeight:        s.Resources.IOWeight,
	}); err != nil {
		// A limit that could not be applied is reported, not ignored: the
		// operator believes the limit is in place.
		fmt.Fprintf(cfg.Stderr, "warning: some resource limits were not applied: %v\n", err)
	}

	// --- the re-exec ---
	payload, err := encodeSpec(s, absRoot)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("/proc/self/exe", InitArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Stdin, cfg.Stdout, cfg.Stderr
	// The child receives the spec through the environment rather than argv,
	// so it does not appear in the host's process list.
	cmd.Env = []string{specEnvVar + "=" + payload}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags(s.Namespaces),
		// Without this the container's mounts propagate back to the host,
		// which defeats the mount namespace entirely. It is the single most
		// commonly forgotten flag.
		Unshareflags: syscall.CLONE_NEWNS,
		// Put the child in its own process group so a Ctrl-C in the terminal
		// does not race the parent's cleanup.
		Setpgid: true,
	}
	if s.Namespaces.User {
		// Map container root to an unprivileged host id. This is what allows
		// a rootless container: the process is uid 0 inside and harmless
		// outside.
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: s.HostUID, Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: s.HostGID, Size: 1},
		}
		// Required since Linux 3.19: without it, setgroups inside the
		// namespace could be used to drop a group and gain access.
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("container: start: %w", err)
	}

	// The pid joins the cgroup *after* clone but *before* the workload execs,
	// which is the only window where limits can be applied to it. Any earlier
	// and there is no pid; any later and the workload has already run
	// unlimited.
	if err := group.AddProcess(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("container: move process into cgroup: %w", err)
	}

	// Signal the child that setup is done and it may proceed.
	// (The child waits by reading its spec, which is already present, so no
	// extra pipe is needed here; the ordering above is what matters.)

	result := &Result{}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeout <-chan time.Time
	if s.Timeout > 0 {
		t := time.NewTimer(s.Timeout)
		defer t.Stop()
		timeout = t.C
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	for waiting := true; waiting; {
		select {
		case err := <-done:
			result.ExitCode, result.Signal = exitInfo(err, cmd)
			waiting = false
		case <-timeout:
			result.TimedOut = true
			killTree(cmd.Process.Pid)
			<-done
			result.ExitCode = 124 // conventional timeout exit code
			waiting = false
		case <-sigs:
			// Forward the signal and let the child exit on its own terms.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				result.ExitCode, result.Signal = exitInfo(err, cmd)
			case <-time.After(10 * time.Second):
				killTree(cmd.Process.Pid)
				<-done
				result.ExitCode = 137
			}
			waiting = false
		}
	}

	result.Duration = time.Since(started)
	// Read the cgroup before deleting it, or the counters are gone.
	result.Stats = group.Stats()
	result.OOMKilled = result.Stats.OOMKills > 0

	if layers != nil {
		if changed, err := layers.Changed(); err == nil && len(changed) > 0 {
			fmt.Fprintf(cfg.Stderr, "container wrote %d path(s) to its upper layer\n", len(changed))
		}
	}
	return result, nil
}

func exitInfo(err error, cmd *exec.Cmd) (code, sig int) {
	if err == nil {
		return 0, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal()), int(ws.Signal())
			}
			return ws.ExitStatus(), 0
		}
		return ee.ExitCode(), 0
	}
	return 1, 0
}

// killTree kills the process group, not just the leader. With a pid
// namespace, killing pid 1 inside takes the whole namespace with it; without
// one, children would survive.
func killTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

const specEnvVar = "MINI_CONTAINER_SPEC"

func encodeSpec(s *spec.Spec, absRoot string) (string, error) {
	clone := *s
	clone.RootFS = absRoot
	clone.Image = ""
	return marshalSpec(&clone)
}

// pidString is a helper used in diagnostics.
func pidString(p *os.Process) string {
	if p == nil {
		return "?"
	}
	return strconv.Itoa(p.Pid)
}
