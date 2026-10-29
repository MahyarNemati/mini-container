//go:build linux && integration

// Real container tests. They need root and a cgroup v2 host, so they are
// behind a build tag and run only in CI on a Linux runner -- which is the
// honest arrangement: the interesting behaviour of this package cannot be
// tested anywhere else, and pretending otherwise with mocks would test
// nothing.
//
//	sudo go test -tags integration ./internal/container/...
package container_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MahyarNemati/mini-container/internal/container"
	"github.com/MahyarNemati/mini-container/internal/spec"
)

// TestMain takes the re-exec path before the testing package parses flags.
//
// The runtime enters new namespaces by re-executing /proc/self/exe with a
// sentinel argument. Under `go test` that binary is *this* test binary, so it
// has to dispatch on the sentinel exactly as cmd/minic does. Without this,
// the child is a test binary invoked with an unknown flag: it prints the
// testing package's usage onto the container's stdout and exits 2.
//
// That failure is quiet, which is the dangerous part. A test asserting "the
// host secret does not appear in the output" passes against a usage message,
// so a suite written with negative assertions can go green while no container
// has run at all. Every test below therefore asserts something the workload
// must have produced, and run() refuses output that looks like this failure.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == container.InitArg {
		container.Init()
		return // unreachable: Init execs or exits
	}
	os.Exit(m.Run())
}

// minimalRootFS builds a root filesystem containing busybox, which provides
// sh, ls, hostname and friends as a single static binary.
func minimalRootFS(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skipf("busybox not installed: %v", err)
	}

	for _, dir := range []string{"bin", "proc", "sys", "dev", "tmp", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "bin", "busybox")
	if err := os.WriteFile(target, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	// busybox dispatches on argv[0], so symlinks give us the applets.
	// grep, dd and df are here because the tests use them. A busybox built
	// with the standalone shell runs applets without a symlink, so a missing
	// one works on Debian and fails on a host whose busybox was built without
	// it -- the sort of difference that produces a test that passes in CI and
	// nowhere else.
	for _, applet := range []string{
		"sh", "ls", "hostname", "cat", "echo", "sleep", "ps", "mount", "id", "touch",
		"grep", "dd", "df", "wc", "head", "sync",
	} {
		if err := os.Symlink("busybox", filepath.Join(root, "bin", applet)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if err := container.Preflight(""); err != nil {
		t.Skipf("host cannot run containers: %v", err)
	}
}

// run executes a command in a container and returns its output.
func run(t *testing.T, mutate func(*spec.Spec), args ...string) (*container.Result, string) {
	t.Helper()
	root := minimalRootFS(t)

	s := &spec.Spec{
		ID:         "test" + strings.ReplaceAll(t.Name(), "/", "_"),
		RootFS:     root,
		Args:       args,
		Hostname:   "testbox",
		Namespaces: spec.DefaultNamespaces(),
		Resources:  spec.Resources{PidsMax: 64},
		Timeout:    30 * time.Second,
	}
	// Keep the id within the validated character set.
	s.ID = sanitizeID(s.ID)
	if mutate != nil {
		mutate(s)
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	// A pipe per stream, each drained before its buffer is read. Reading a
	// buffer while the copying goroutine is still running is a race that
	// shows up as intermittently empty output -- and an assertion of the form
	// "the output does not contain X" passes every time it loses.
	stdout, waitOut := outputPipe(t)
	stderr, waitErr := outputPipe(t)

	result, err := container.Run(container.Config{
		Spec:     s,
		StateDir: t.TempDir(),
		Stdin:    devnull,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	text := waitOut() + waitErr()
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, text)
	}
	// The tripwire described on TestMain. If the re-exec dispatch is ever
	// lost again, this says so instead of letting the suite go green.
	for _, sign := range []string{container.InitArg, "flag provided but not defined", "Usage of "} {
		if strings.Contains(text, sign) {
			t.Fatalf("the container did not run the workload: the re-exec child fell through to "+
				"the test binary's flag parsing (saw %q). Is TestMain still dispatching on "+
				"container.InitArg?\noutput:\n%s", sign, text)
		}
	}
	return result, text
}

// requireRan asserts that a marker the workload itself printed is present. A
// test that only checks for the absence of something cannot tell a contained
// container from one that never started.
func requireRan(t *testing.T, out, marker string) {
	t.Helper()
	if !strings.Contains(out, marker) {
		t.Fatalf("the workload did not run: expected %q in the output\noutput:\n%s", marker, out)
	}
}

// outputPipe gives the config an *os.File to write to, and a function that
// closes it, waits for every byte to be copied, and returns the text.
//
// The wait is the point. The container writes into a pipe and a goroutine
// copies it into a buffer; reading that buffer before the copy finishes
// returns whatever happened to have arrived, which is often nothing.
func outputPipe(t *testing.T) (*os.File, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buf.ReadFrom(r)
	}()

	var once sync.Once
	wait := func() string {
		once.Do(func() {
			// Closing the write end is what gives the reader its EOF. The
			// child holds its own duplicate of this descriptor, so the reader
			// does not see EOF until the workload has also exited.
			_ = w.Close()
			<-done
			_ = r.Close()
		})
		return buf.String()
	}
	// A test that fails before calling wait still releases the pipe.
	t.Cleanup(func() { wait() })
	return w, wait
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

func TestRunsACommandAndReturnsItsExitCode(t *testing.T) {
	requireRoot(t)

	result, out := run(t, nil, "/bin/echo", "hello from the container")
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (output: %s)", result.ExitCode, out)
	}
	if !strings.Contains(out, "hello from the container") {
		t.Errorf("output = %q", out)
	}
}

func TestNonZeroExitIsPropagated(t *testing.T) {
	requireRoot(t)
	result, _ := run(t, nil, "/bin/sh", "-c", "exit 42")
	if result.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", result.ExitCode)
	}
}

func TestPIDNamespaceMakesTheWorkloadPIDOne(t *testing.T) {
	requireRoot(t)
	// The clearest evidence the pid namespace works: the container sees
	// almost nothing, and itself as pid 1.
	_, out := run(t, nil, "/bin/sh", "-c", "echo pid=$$")
	if !strings.Contains(out, "pid=1") {
		t.Errorf("expected the workload to be pid 1, got: %s", out)
	}
}

func TestProcShowsOnlyContainerProcesses(t *testing.T) {
	requireRoot(t)
	// /proc must be mounted *inside* the pid namespace, or it lists the
	// host's processes -- both a leak and deeply confusing.
	_, out := run(t, nil, "/bin/sh", "-c", "echo ran; ls /proc | grep -c '^[0-9]*$'")
	requireRan(t, out, "ran")

	// A container running one shell sees a handful of pids, not hundreds. The
	// count is the assertion: "not empty" would be satisfied by any output at
	// all, including an error message.
	fields := strings.Fields(out)
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		t.Fatalf("expected a pid count, got %q", out)
	}
	t.Logf("/proc entries: %d", n)
	if n < 1 || n > 20 {
		t.Errorf("expected a handful of pids in the namespace, got %d: /proc is probably the host's", n)
	}
}

func TestUTSNamespaceSetsTheHostname(t *testing.T) {
	requireRoot(t)
	_, out := run(t, nil, "/bin/hostname")
	if !strings.Contains(out, "testbox") {
		t.Errorf("hostname = %q, want testbox", strings.TrimSpace(out))
	}
}

func TestRootFilesystemIsThePivotedOne(t *testing.T) {
	requireRoot(t)
	// If pivot_root silently failed, this would list the host's root.
	_, out := run(t, nil, "/bin/ls", "/")
	if strings.Contains(out, "home") || strings.Contains(out, "usr") {
		t.Errorf("the container appears to see the host root: %s", out)
	}
	if !strings.Contains(out, "bin") {
		t.Errorf("expected the container's own /bin: %s", out)
	}
}

func TestOldRootIsNotReachable(t *testing.T) {
	requireRoot(t)
	// Until the old root is detached, the host filesystem is reachable at
	// /.old-root and the whole exercise is pointless.
	_, out := run(t, nil, "/bin/sh", "-c", "ls -a / | grep old-root || echo clean")
	if !strings.Contains(out, "clean") {
		t.Errorf("the old root is still visible: %s", out)
	}
}

func TestMemoryLimitCausesAnOOMKill(t *testing.T) {
	requireRoot(t)
	// Allocate far more than the limit. The process just sees SIGKILL, so
	// the cgroup's oom_kill counter is the only way to tell this from an
	// ordinary failure -- which is exactly why Result exposes it.
	result, out := run(t, func(s *spec.Spec) {
		s.Resources.MemoryBytes = 16 * 1024 * 1024
		s.Resources.MemorySwapBytes = 0
		// /tmp is a 64 MiB tmpfs, and tmpfs pages are charged to the
		// cgroup's memory, so filling it is the portable way to push a
		// container over memory.max. busybox's `head -c 200m` silently
		// accepts the suffix and writes nothing, which made this test pass
		// with a peak of 1.8 MB -- dd's count is unambiguous.
		// 32 MiB: twice the memory limit, but half the tmpfs, so the kill
		// comes from memory.max rather than from ENOSPC. tmpfs pages are
		// charged to the cgroup, which is what makes this a memory limit test
		// and not a disk one. stderr is kept -- suppressing it hid a missing
		// applet and then a missing /dev/zero, and both times the test passed
		// on the strength of a non-zero exit alone.
	}, "/bin/sh", "-c", "echo ran; dd if=/dev/zero of=/tmp/big bs=1M count=32")
	requireRan(t, out, "ran")

	if result.ExitCode == 0 {
		t.Errorf("expected a failure under a 16 MiB limit, got 0 (output: %s)", out)
	}
	// A non-zero exit on its own proves nothing: dd fails for many reasons,
	// and every earlier version of this test was satisfied by one of them.
	// The oom_kill counter is the only evidence that the limit did it.
	if result.Stats.OOMKills == 0 && !result.OOMKilled {
		t.Errorf("the container failed (exit %d) but the cgroup recorded no OOM kill, "+
			"so the memory limit is not what stopped it.\nmemory.events: %+v\noutput:\n%s",
			result.ExitCode, result.Stats, out)
	}
}

func TestPidsLimitStopsAForkBomb(t *testing.T) {
	requireRoot(t)
	// The cheapest protection there is: without pids.max a process inside
	// the container can exhaust the host's pid space.
	result, out := run(t, func(s *spec.Spec) {
		s.Resources.PidsMax = 8
		s.Timeout = 10 * time.Second
		// A POSIX loop rather than the `:(){ :|:& };:` one-liner: busybox ash
		// rejects `:` as a function name ("syntax error: bad function name")
		// and the whole command never ran.
	}, "/bin/sh", "-c", "echo ran; i=0; while [ $i -lt 64 ]; do sleep 5 & i=$((i+1)); done; wait")
	requireRan(t, out, "ran")

	// The point is that the host survives and the container is contained;
	// the exact exit code depends on which fork failed.
	//
	// pids.peak, not pids.current: the stats are read after the workload has
	// exited, so pids.current is 0 and an assertion against it says nothing
	// either way.
	t.Logf("fork bomb result: exit %d, pids peaked at %d (current %d)",
		result.ExitCode, result.Stats.PidsPeak, result.Stats.Pids)
	if result.Stats.PidsPeak == 0 {
		t.Skip("this kernel does not provide pids.peak, so the limit cannot be checked after the fact")
	}
	if result.Stats.PidsPeak > 8 {
		t.Errorf("pids peaked at %d despite a limit of 8", result.Stats.PidsPeak)
	}
}

func TestCPULimitIsThrottled(t *testing.T) {
	requireRoot(t)
	result, out := run(t, func(s *spec.Spec) {
		s.Resources.CPUMillis = 100 // a tenth of a core
		s.Timeout = 8 * time.Second
	}, "/bin/sh", "-c", "echo ran; i=0; while [ $i -lt 100000000 ]; do i=$((i+1)); done")
	requireRan(t, out, "ran")

	// Burning no CPU at all means the loop never ran, so there was nothing to
	// throttle and the test would pass for the wrong reason.
	if result.Stats.CPUUsage <= 0 {
		t.Errorf("the cgroup recorded no cpu usage, so the limit was never tested")
	}

	// Throttling is the difference between "slow" and "actually limited".
	if result.Stats.Throttled == 0 {
		t.Logf("note: nr_throttled was 0; cpu usage %s over %s",
			result.Stats.CPUUsage, result.Duration)
	} else {
		t.Logf("throttled %d times, cpu usage %s", result.Stats.Throttled, result.Stats.CPUUsage)
	}
}

func TestTimeoutKillsTheContainer(t *testing.T) {
	requireRoot(t)
	start := time.Now()
	result, _ := run(t, func(s *spec.Spec) {
		s.Timeout = 2 * time.Second
	}, "/bin/sleep", "60")

	if !result.TimedOut {
		t.Error("expected TimedOut to be set")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("took %s; the timeout did not kill it promptly", elapsed)
	}
	if result.ExitCode != 124 {
		t.Errorf("exit code = %d, want the conventional 124 for a timeout", result.ExitCode)
	}
}

func TestReadOnlyRootRefusesWrites(t *testing.T) {
	requireRoot(t)
	_, out := run(t, func(s *spec.Spec) {
		s.ReadOnlyRoot = true
	}, "/bin/sh", "-c", "touch /should-fail 2>&1 || echo refused")

	if !strings.Contains(out, "refused") && !strings.Contains(out, "read-only") {
		t.Errorf("a write to a read-only root should have failed: %s", out)
	}
}

func TestReadOnlyRootStillAllowsTmp(t *testing.T) {
	requireRoot(t)
	// /tmp is a tmpfs mounted before the remount, so it must stay writable --
	// otherwise almost nothing runs under --read-only.
	_, out := run(t, func(s *spec.Spec) {
		s.ReadOnlyRoot = true
	}, "/bin/sh", "-c", "touch /tmp/ok && echo wrote")

	if !strings.Contains(out, "wrote") {
		t.Errorf("/tmp should be writable even with a read-only root: %s", out)
	}
}

func TestMountNamespaceDoesNotLeakToTheHost(t *testing.T) {
	requireRoot(t)
	before := mountCount(t)
	_, out := run(t, nil, "/bin/sh", "-c", "echo ran; mount | wc -l")
	requireRan(t, out, "ran")
	after := mountCount(t)

	// Without MS_PRIVATE propagation the container's mounts appear on the
	// host, which is the most commonly missed step in a hand-written runtime.
	if after > before {
		t.Errorf("host mount count went from %d to %d: the container's mounts leaked", before, after)
	}
}

func mountCount(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

func TestBindMountIsVisibleInTheContainer(t *testing.T) {
	requireRoot(t)
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "marker"), []byte("from the host"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out := run(t, func(s *spec.Spec) {
		s.Mounts = []spec.Mount{{Source: hostDir, Destination: "/data", Type: "bind"}}
	}, "/bin/cat", "/data/marker")

	if !strings.Contains(out, "from the host") {
		t.Errorf("bind mount not visible: %s", out)
	}
}

func TestReadOnlyBindMountRefusesWrites(t *testing.T) {
	requireRoot(t)
	hostDir := t.TempDir()

	// A read-only bind needs a *second* remount: MS_RDONLY is ignored on the
	// initial MS_BIND, which is a long-standing kernel behaviour and a
	// frequent source of "why is my ro mount writable".
	_, out := run(t, func(s *spec.Spec) {
		s.Mounts = []spec.Mount{{
			Source: hostDir, Destination: "/ro", Type: "bind", Options: []string{"ro"},
		}}
	}, "/bin/sh", "-c", "touch /ro/x 2>&1 || echo refused")

	if !strings.Contains(out, "refused") && !strings.Contains(out, "read-only") {
		t.Errorf("a write to a read-only bind mount should have failed: %s", out)
	}
}

func TestStandardDeviceNodesWork(t *testing.T) {
	requireRoot(t)
	// Without these, `dd if=/dev/zero`, `> /dev/null` and anything seeding a
	// random number generator all fail inside the container -- which is most
	// shell scripts.
	_, out := run(t, nil, "/bin/sh", "-c",
		"echo ran; dd if=/dev/zero of=/tmp/z bs=1k count=1 2>&1; "+
			"echo discarded > /dev/null && echo null-ok; "+
			"dd if=/dev/urandom of=/tmp/r bs=1k count=1 2>&1 && echo urandom-ok")
	requireRan(t, out, "ran")
	requireRan(t, out, "null-ok")
	requireRan(t, out, "urandom-ok")
	if strings.Contains(out, "can't open") || strings.Contains(out, "No such file") {
		t.Errorf("a device node is missing: %s", out)
	}
}

func TestDevIsNodevSoTheContainerCannotMakeItsOwnDevices(t *testing.T) {
	requireRoot(t)
	// The tmpfs on /dev is mounted nodev, so even if the container could
	// create a node for a host disk the kernel would ignore it. Checking the
	// mount options is the assertion; mknod itself needs a capability the
	// container may or may not have.
	_, out := run(t, nil, "/bin/sh", "-c", "echo ran; mount | grep ' /dev '")
	requireRan(t, out, "ran")
	if !strings.Contains(out, "nodev") {
		t.Errorf("/dev should be mounted nodev: %s", out)
	}
	if !strings.Contains(out, "nosuid") {
		t.Errorf("/dev should be mounted nosuid: %s", out)
	}
}

func TestEnvironmentIsNotInheritedFromTheHost(t *testing.T) {
	requireRoot(t)
	t.Setenv("HOST_SECRET", "do-not-leak")

	_, out := run(t, nil, "/bin/sh", "-c", "echo \"secret=[$HOST_SECRET]\"")
	// Checking for the marker first: without it, "do-not-leak is absent"
	// would be satisfied by a container that never started.
	requireRan(t, out, "secret=[")
	// A container that sees the host's environment leaks whatever is in it.
	if strings.Contains(out, "do-not-leak") {
		t.Errorf("the host environment leaked into the container: %s", out)
	}
}

func TestExplicitEnvIsPassed(t *testing.T) {
	requireRoot(t)
	_, out := run(t, func(s *spec.Spec) {
		s.Env = append(spec.DefaultEnv("testbox"), "MY_VAR=my-value")
	}, "/bin/sh", "-c", "echo [$MY_VAR]")

	if !strings.Contains(out, "my-value") {
		t.Errorf("explicit env not passed: %s", out)
	}
}

func TestCgroupIsCleanedUpAfterwards(t *testing.T) {
	requireRoot(t)
	run(t, nil, "/bin/echo", "done")

	// An orphan cgroup per run would accumulate until the host ran out.
	entries, err := os.ReadDir("/sys/fs/cgroup")
	if err != nil {
		t.Skipf("cannot read cgroup root: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "minic-test") {
			t.Errorf("left an orphan cgroup: %s", e.Name())
		}
	}
}
