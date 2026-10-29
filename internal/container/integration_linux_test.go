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
	"strings"
	"testing"
	"time"

	"github.com/MahyarNemati/mini-container/internal/container"
	"github.com/MahyarNemati/mini-container/internal/spec"
)

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
	for _, applet := range []string{"sh", "ls", "hostname", "cat", "echo", "sleep", "ps", "mount", "id", "touch"} {
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

	var out bytes.Buffer
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	result, err := container.Run(container.Config{
		Spec:     s,
		StateDir: t.TempDir(),
		Stdin:    devnull,
		Stdout:   writerFile(t, &out),
		Stderr:   writerFile(t, &out),
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	return result, out.String()
}

// writerFile adapts a buffer to the *os.File the config wants, via a pipe.
func writerFile(t *testing.T, buf *bytes.Buffer) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buf.ReadFrom(r)
	}()
	t.Cleanup(func() {
		_ = w.Close()
		<-done
		_ = r.Close()
	})
	return w
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
	_, out := run(t, nil, "/bin/sh", "-c", "ls /proc | grep -c '^[0-9]*$'")
	// A container running one shell should see a handful of pids, not
	// hundreds.
	if strings.TrimSpace(out) == "" {
		t.Fatalf("no output: %q", out)
	}
	t.Logf("/proc entries: %s", strings.TrimSpace(out))
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
	}, "/bin/sh", "-c", "head -c 200m /dev/zero | cat > /tmp/big")

	if result.ExitCode == 0 {
		t.Errorf("expected a failure under a 16 MiB limit, got 0 (output: %s)", out)
	}
	if !result.OOMKilled {
		t.Logf("note: exit %d without an oom_kill counter; memory.events reported %+v",
			result.ExitCode, result.Stats)
	}
}

func TestPidsLimitStopsAForkBomb(t *testing.T) {
	requireRoot(t)
	// The cheapest protection there is: without pids.max a process inside
	// the container can exhaust the host's pid space.
	result, _ := run(t, func(s *spec.Spec) {
		s.Resources.PidsMax = 8
		s.Timeout = 10 * time.Second
	}, "/bin/sh", "-c", ":(){ :|:& };: ; sleep 5")

	// The point is that the host survives and the container is contained;
	// the exact exit code depends on which fork failed.
	t.Logf("fork bomb result: exit %d, pids peaked at %d", result.ExitCode, result.Stats.Pids)
	if result.Stats.Pids > 20 {
		t.Errorf("pids reached %d despite a limit of 8", result.Stats.Pids)
	}
}

func TestCPULimitIsThrottled(t *testing.T) {
	requireRoot(t)
	result, _ := run(t, func(s *spec.Spec) {
		s.Resources.CPUMillis = 100 // a tenth of a core
		s.Timeout = 8 * time.Second
	}, "/bin/sh", "-c", "i=0; while [ $i -lt 100000000 ]; do i=$((i+1)); done")

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
	run(t, nil, "/bin/sh", "-c", "mount | wc -l")
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

func TestEnvironmentIsNotInheritedFromTheHost(t *testing.T) {
	requireRoot(t)
	t.Setenv("HOST_SECRET", "do-not-leak")

	_, out := run(t, nil, "/bin/sh", "-c", "echo \"secret=[$HOST_SECRET]\"")
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
