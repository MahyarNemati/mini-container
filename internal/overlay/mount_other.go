//go:build !linux

package overlay

import "errors"

// ErrUnsupportedPlatform is returned for every mount operation off Linux.
//
// These stubs exist so the whole module compiles and the portable logic
// (option formatting, layout preparation, diffing) can be developed and
// tested on any machine. Only the syscalls are Linux specific, and pretending
// otherwise with build-tag-free code would mean the package could not be
// compiled at all during development.
var ErrUnsupportedPlatform = errors.New("overlay: overlayfs is only available on Linux")

// Mount is unsupported off Linux.
func (l *Layout) Mount() error { return ErrUnsupportedPlatform }

// Unmount is unsupported off Linux.
func (l *Layout) Unmount() error { return ErrUnsupportedPlatform }

// Supported reports that overlayfs is unavailable.
func Supported() error { return ErrUnsupportedPlatform }
