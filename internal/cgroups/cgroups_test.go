package cgroups_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MahyarNemati/mini-container/internal/cgroups"
)

// fakeRoot builds a directory that looks enough like a cgroup v2 hierarchy to
// exercise the file-writing logic on any OS. This is why Group takes an
// injectable root: the interface is "write text to files", so the logic is
// testable without a Linux kernel.
func fakeRoot(t *testing.T, controllers string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), controllers)
	write(t, filepath.Join(root, "cgroup.subtree_control"), "")
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedGroupFiles pre-creates the files Apply writes, since the kernel creates
// them automatically and a plain directory does not.
func seedGroupFiles(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{
		"memory.max", "memory.swap.max", "cpu.max", "pids.max", "io.weight", "cgroup.procs",
	} {
		write(t, filepath.Join(dir, f), "")
	}
}

func TestCPUMaxFormat(t *testing.T) {
	// The format is "<quota> <period>" in microseconds, which is easy to get
	// wrong and invisible when you do: the kernel accepts a plausible-looking
	// wrong number.
	for _, tc := range []struct {
		millis int64
		want   string
	}{
		{1000, "100000 100000"}, // one core
		{500, "50000 100000"},   // half a core
		{1500, "150000 100000"}, // one and a half
		{50, "5000 100000"},
		{0, "max 100000"},
		{-1, "max 100000"},
	} {
		if got := cgroups.CPUMax(tc.millis); got != tc.want {
			t.Errorf("CPUMax(%d) = %q, want %q", tc.millis, got, tc.want)
		}
	}
}

func TestAvailableChecksForCgroupV2(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	if err := cgroups.Available(root); err != nil {
		t.Errorf("Available: %v", err)
	}

	// A v1 hierarchy has no cgroup.controllers, which is the cheapest and
	// most reliable way to tell the versions apart.
	bare := t.TempDir()
	if err := cgroups.Available(bare); err == nil {
		t.Error("a directory without cgroup.controllers should not look like cgroup v2")
	}
}

func TestControllers(t *testing.T) {
	root := fakeRoot(t, "cpuset cpu io memory pids")
	got, err := cgroups.Controllers(root)
	if err != nil {
		t.Fatalf("Controllers: %v", err)
	}
	if len(got) != 5 || got[0] != "cpuset" {
		t.Errorf("Controllers = %v", got)
	}
}

func TestEnableControllersWritesSubtreeControl(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	if err := cgroups.EnableControllers(root, "memory", "cpu"); err != nil {
		t.Fatalf("EnableControllers: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read subtree_control: %v", err)
	}
	// The "+" prefix is what enables rather than disables; writing bare names
	// is a silent no-op.
	for _, want := range []string{"+memory", "+cpu"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("subtree_control = %q, want it to contain %q", raw, want)
		}
	}
}

func TestEnableControllersReportsMissingOnes(t *testing.T) {
	// This is the failure people hit: the controller is not available, the
	// write silently does nothing, and memory.max never appears in the child
	// group -- so the limit the operator set does not exist.
	root := fakeRoot(t, "cpu pids")
	err := cgroups.EnableControllers(root, "memory", "cpu")
	if err == nil {
		t.Fatal("expected an error when a requested controller is unavailable")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("error should name the missing controller, got %v", err)
	}
}

func TestNewRejectsNamesThatWouldEscapeTheRoot(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	for _, name := range []string{"", "..", ".", "a/b", "a\\b", "../escape"} {
		if _, err := cgroups.New(root, name); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

func TestApplyWritesEveryLimit(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids io")
	g, err := cgroups.New(root, "test-container")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seedGroupFiles(t, g.Path())

	if err := g.Apply(cgroups.Limits{
		MemoryBytes:     64 * 1024 * 1024,
		MemorySwapBytes: 0,
		CPUMillis:       500,
		PidsMax:         32,
		IOWeight:        100,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for file, want := range map[string]string{
		"memory.max":      "67108864",
		"memory.swap.max": "0",
		"cpu.max":         "50000 100000",
		"pids.max":        "32",
		"io.weight":       "100",
	} {
		raw, err := os.ReadFile(filepath.Join(g.Path(), file))
		if err != nil {
			t.Errorf("read %s: %v", file, err)
			continue
		}
		if strings.TrimSpace(string(raw)) != want {
			t.Errorf("%s = %q, want %q", file, raw, want)
		}
	}
}

func TestApplySetsSwapToZeroWithAMemoryLimit(t *testing.T) {
	// Without this, a container hitting its memory limit thrashes in swap
	// instead of being OOM killed -- slower and much harder to diagnose.
	root := fakeRoot(t, "memory")
	g, _ := cgroups.New(root, "swap-test")
	seedGroupFiles(t, g.Path())

	if err := g.Apply(cgroups.Limits{MemoryBytes: 1024 * 1024}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(g.Path(), "memory.swap.max"))
	if strings.TrimSpace(string(raw)) != "0" {
		t.Errorf("memory.swap.max = %q, want 0", raw)
	}
}

func TestApplySkipsZeroLimits(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	g, _ := cgroups.New(root, "partial")
	seedGroupFiles(t, g.Path())

	if err := g.Apply(cgroups.Limits{PidsMax: 16}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Only pids should have been written; a zero means "do not set", not
	// "set to zero", which for memory.max would make the container unable to
	// allocate anything.
	raw, _ := os.ReadFile(filepath.Join(g.Path(), "memory.max"))
	if strings.TrimSpace(string(raw)) != "" {
		t.Errorf("memory.max = %q, want it untouched", raw)
	}
}

func TestApplyReportsAMissingController(t *testing.T) {
	root := fakeRoot(t, "pids")
	g, _ := cgroups.New(root, "no-memory")
	// Deliberately do not create memory.max, as a kernel without the
	// controller would not.
	write(t, filepath.Join(g.Path(), "pids.max"), "")

	err := g.Apply(cgroups.Limits{MemoryBytes: 1 << 20, PidsMax: 8})
	if err == nil {
		t.Fatal("a limit that could not be applied must be reported, not ignored")
	}
}

func TestAddProcessWritesToCgroupProcs(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	g, _ := cgroups.New(root, "procs")
	seedGroupFiles(t, g.Path())

	if err := g.AddProcess(4242); err != nil {
		t.Fatalf("AddProcess: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(g.Path(), "cgroup.procs"))
	if strings.TrimSpace(string(raw)) != "4242" {
		t.Errorf("cgroup.procs = %q, want 4242", raw)
	}
}

func TestStatsParsesKeyedFiles(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids")
	g, _ := cgroups.New(root, "stats")

	write(t, filepath.Join(g.Path(), "memory.current"), "1048576\n")
	write(t, filepath.Join(g.Path(), "memory.peak"), "2097152\n")
	write(t, filepath.Join(g.Path(), "pids.current"), "7\n")
	write(t, filepath.Join(g.Path(), "memory.events"), "low 0\nhigh 0\nmax 3\noom 1\noom_kill 2\n")
	write(t, filepath.Join(g.Path(), "cpu.stat"), "usage_usec 1500000\nnr_periods 10\nnr_throttled 4\n")

	s := g.Stats()
	if s.MemoryCurrent != 1048576 || s.MemoryPeak != 2097152 || s.Pids != 7 {
		t.Errorf("stats = %+v", s)
	}
	// oom_kill is the only reliable way to distinguish an OOM kill from an
	// ordinary SIGKILL, since the process sees the same thing either way.
	if s.OOMKills != 2 {
		t.Errorf("OOMKills = %d, want 2", s.OOMKills)
	}
	if s.CPUUsage.Microseconds() != 1500000 {
		t.Errorf("CPUUsage = %s, want 1.5s", s.CPUUsage)
	}
	// Throttling is the difference between "slow" and "actually limited".
	if s.Throttled != 4 {
		t.Errorf("Throttled = %d, want 4", s.Throttled)
	}
}

func TestStatsToleratesMissingFiles(t *testing.T) {
	root := fakeRoot(t, "pids")
	g, _ := cgroups.New(root, "sparse")
	// Which files exist depends on which controllers are enabled, so missing
	// ones must not be an error.
	s := g.Stats()
	if s.MemoryCurrent != 0 || s.OOMKills != 0 {
		t.Errorf("stats = %+v, want zeroes", s)
	}
}

func TestStatsIgnoresMaxAsANumber(t *testing.T) {
	root := fakeRoot(t, "memory")
	g, _ := cgroups.New(root, "maxval")
	// "max" is a legitimate value in these files and is not a number.
	write(t, filepath.Join(g.Path(), "memory.current"), "max\n")
	if got := g.Stats().MemoryCurrent; got != 0 {
		t.Errorf("MemoryCurrent = %d, want 0 for \"max\"", got)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	root := fakeRoot(t, "pids")
	g, _ := cgroups.New(root, "gone")
	if err := g.Delete(); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := g.Delete(); err != nil {
		t.Errorf("second Delete should be a no-op, got %v", err)
	}
}
