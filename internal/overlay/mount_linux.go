//go:build linux

package overlay

import (
	"fmt"
	"os"
	"syscall"
)

// Mount performs the overlayfs mount.
func (l *Layout) Mount() error {
	opts, err := l.MountOptions()
	if err != nil {
		return err
	}
	if err := syscall.Mount("overlay", l.Merged, "overlay", 0, opts); err != nil {
		// EINVAL here is almost always one of three things, and saying so
		// saves an hour: the kernel lacks overlayfs, upper and lower are on
		// different filesystems, or upper/work are not on the same one.
		return fmt.Errorf("overlay: mount %s (%s): %w "+
			"(check that overlayfs is available, and that upperdir and workdir are on the same filesystem)",
			l.Merged, opts, err)
	}
	return nil
}

// Unmount detaches the overlay.
func (l *Layout) Unmount() error {
	// MNT_DETACH: a lazy unmount succeeds even when something still holds a
	// reference, which happens routinely when a process is exiting. A
	// non-lazy unmount would fail here and leave the mount behind forever.
	if err := syscall.Unmount(l.Merged, syscall.MNT_DETACH); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("overlay: unmount %s: %w", l.Merged, err)
	}
	return nil
}

// Supported reports whether the kernel has overlayfs.
func Supported() error {
	raw, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return fmt.Errorf("overlay: read /proc/filesystems: %w", err)
	}
	if !containsWord(string(raw), "overlay") {
		return fmt.Errorf("overlay: the kernel does not support overlayfs")
	}
	return nil
}

func containsWord(haystack, word string) bool {
	for _, line := range splitLines(haystack) {
		for _, f := range splitFields(line) {
			if f == word {
				return true
			}
		}
	}
	return false
}
