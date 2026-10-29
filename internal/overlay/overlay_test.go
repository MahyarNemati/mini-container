package overlay_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MahyarNemati/mini-container/internal/overlay"
)

func TestPrepareCreatesTheDirectories(t *testing.T) {
	base := t.TempDir()
	lower := filepath.Join(base, "image")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Fatal(err)
	}

	l, err := overlay.Prepare(filepath.Join(base, "work"), lower)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, dir := range []string{l.Upper, l.Work, l.Merged} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not created as a directory", dir)
		}
	}
}

func TestPrepareRejectsAMissingLower(t *testing.T) {
	base := t.TempDir()
	if _, err := overlay.Prepare(base, filepath.Join(base, "nope")); err == nil {
		t.Error("a missing lower directory should be rejected")
	}
}

func TestPrepareRejectsAFileAsLower(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := overlay.Prepare(base, file); err == nil {
		t.Error("a regular file should not be accepted as a lower layer")
	}
}

func TestPrepareRequiresALower(t *testing.T) {
	if _, err := overlay.Prepare(t.TempDir()); err == nil {
		t.Error("Prepare with no lower directory should fail")
	}
}

func TestMountOptionsFormat(t *testing.T) {
	// The kernel's parser is unforgiving and the format is the fiddly part,
	// which is why it is separated from the syscall and tested directly.
	l := &overlay.Layout{
		Lower: []string{"/img/a"},
		Upper: "/state/upper",
		Work:  "/state/work",
	}
	got, err := l.MountOptions()
	if err != nil {
		t.Fatalf("MountOptions: %v", err)
	}
	want := "lowerdir=/img/a,upperdir=/state/upper,workdir=/state/work"
	if got != want {
		t.Errorf("MountOptions = %q, want %q", got, want)
	}
}

func TestMountOptionsJoinsLowersWithColonsInPriorityOrder(t *testing.T) {
	l := &overlay.Layout{
		Lower: []string{"/img/top", "/img/mid", "/img/base"},
		Upper: "/u", Work: "/w",
	}
	got, _ := l.MountOptions()
	if !strings.HasPrefix(got, "lowerdir=/img/top:/img/mid:/img/base,") {
		t.Errorf("MountOptions = %q; lowers must be colon separated, highest priority first", got)
	}
}

func TestMountOptionsRejectsPathsTheKernelCannotExpress(t *testing.T) {
	// The option string is comma and colon delimited, so a path containing
	// either is unrepresentable. Failing loudly beats a confusing EINVAL.
	for _, bad := range []string{"/has:colon", "/has,comma"} {
		l := &overlay.Layout{Lower: []string{bad}, Upper: "/u", Work: "/w"}
		if _, err := l.MountOptions(); err == nil {
			t.Errorf("path %q should be rejected", bad)
		}
	}
}

func TestMountOptionsRequiresUpperAndWork(t *testing.T) {
	for _, l := range []*overlay.Layout{
		{Lower: []string{"/a"}, Work: "/w"},
		{Lower: []string{"/a"}, Upper: "/u"},
		{Upper: "/u", Work: "/w"},
	} {
		if _, err := l.MountOptions(); err == nil {
			t.Errorf("incomplete layout %+v should be rejected", l)
		}
	}
}

func TestChangedListsTheUpperLayer(t *testing.T) {
	// This is what `docker diff` reports: a walk of the upper directory,
	// since anything there was created or copied up by the container.
	base := t.TempDir()
	lower := filepath.Join(base, "image")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := overlay.Prepare(filepath.Join(base, "state"), lower)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(l.Upper, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Upper, "etc", "hosts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := l.Changed()
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	joined := strings.Join(changed, " ")
	for _, want := range []string{"/etc", "/etc/hosts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Changed = %v, want it to include %q", changed, want)
		}
	}
}

func TestChangedIsEmptyForAFreshOverlay(t *testing.T) {
	base := t.TempDir()
	lower := filepath.Join(base, "image")
	_ = os.MkdirAll(lower, 0o755)
	l, _ := overlay.Prepare(filepath.Join(base, "state"), lower)

	changed, err := l.Changed()
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("Changed = %v, want empty", changed)
	}
}
