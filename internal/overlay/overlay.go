// Package overlay assembles a container root filesystem with overlayfs.
//
// This is how a container starts instantly from a multi-hundred-megabyte
// image without copying it. overlayfs presents a union of:
//
//	lowerdir  the image, read-only and shared between every container
//	upperdir  this container's writes
//	workdir   scratch space the kernel needs for atomic rename
//	merged    the view the container sees
//
// A write to a file that exists only in the lower layer triggers
// **copy-up**: the kernel copies the whole file into the upper layer first.
// That is why a container that appends one byte to a 2 GB log file from the
// image pauses for seconds, and why image layers should not contain large
// mutable files.
//
// Deleting a lower-layer file creates a **whiteout**: a character device with
// major/minor 0/0 in the upper layer. So `rm` inside a container does not free
// space; it uses a little. Both behaviours surprise people, which is why they
// are documented here rather than discovered later.
package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layout is the set of directories an overlay mount needs.
type Layout struct {
	// Lower is the read-only image directory, or several, highest priority
	// first.
	Lower  []string
	Upper  string
	Work   string
	Merged string
}

// ErrInvalid is returned for an unusable layout.
var ErrInvalid = errors.New("overlay: invalid layout")

// Prepare creates the upper, work and merged directories under base and
// returns the layout. The lower directories must already exist.
func Prepare(base string, lower ...string) (*Layout, error) {
	if len(lower) == 0 {
		return nil, fmt.Errorf("%w: at least one lower directory is required", ErrInvalid)
	}
	for _, l := range lower {
		info, err := os.Stat(l)
		if err != nil {
			return nil, fmt.Errorf("%w: lower %s: %v", ErrInvalid, l, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: lower %s is not a directory", ErrInvalid, l)
		}
	}

	l := &Layout{
		Lower:  lower,
		Upper:  filepath.Join(base, "upper"),
		Work:   filepath.Join(base, "work"),
		Merged: filepath.Join(base, "merged"),
	}
	for _, dir := range []string{l.Upper, l.Work, l.Merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("overlay: create %s: %w", dir, err)
		}
	}
	return l, nil
}

// MountOptions renders the option string for mount(2).
//
// Exported and separated from the syscall so it can be tested on any OS --
// the formatting is the fiddly part, and the kernel's parser is unforgiving:
// lowerdir entries are colon separated in priority order, and a path
// containing a colon or comma cannot be expressed at all.
func (l *Layout) MountOptions() (string, error) {
	if len(l.Lower) == 0 || l.Upper == "" || l.Work == "" {
		return "", fmt.Errorf("%w: lower, upper and work are all required", ErrInvalid)
	}
	for _, p := range append(append([]string{}, l.Lower...), l.Upper, l.Work) {
		// The kernel's option parser splits on these, so a path containing
		// one is unrepresentable. Failing loudly beats a confusing EINVAL.
		if strings.ContainsAny(p, ":,") {
			return "", fmt.Errorf("%w: path %q contains ':' or ',', which overlayfs options cannot express", ErrInvalid, p)
		}
	}
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(l.Lower, ":"), l.Upper, l.Work), nil
}

// Changed lists the paths this container has written, relative to the root.
//
// This is what `docker diff` reports, and it is simply a walk of the upper
// directory: anything present there has been created or copied up. Whiteouts
// (deletions) appear as character devices with device number 0.
func (l *Layout) Changed() ([]string, error) {
	var out []string
	err := filepath.WalkDir(l.Upper, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == l.Upper {
			return nil
		}
		rel, err := filepath.Rel(l.Upper, path)
		if err != nil {
			return err
		}
		out = append(out, "/"+rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("overlay: walk upper: %w", err)
	}
	return out, nil
}
