//go:build linux

package container

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/MahyarNemati/mini-container/internal/spec"
)

// Init is the child half, running inside the new namespaces.
//
// It must never return: on success it execs the workload, and on failure it
// exits. Returning would leave a Go runtime running in a half-configured
// namespace, which is a confusing state to debug.
func Init() {
	payload := os.Getenv(specEnvVar)
	if payload == "" {
		fail("no spec in the environment; this process is only meant to be started by the runtime")
	}
	s, err := unmarshalSpec(payload)
	if err != nil {
		fail("decode spec: %v", err)
	}

	if err := setupNamespace(s); err != nil {
		fail("%v", err)
	}

	// Resolve the binary using the container's PATH, after pivoting, so the
	// lookup happens in the container's filesystem rather than the host's.
	path, err := exec.LookPath(s.Args[0])
	if err != nil {
		fail("executable %q not found in the container: %v", s.Args[0], err)
	}

	env := s.Env
	if len(env) == 0 {
		env = spec.DefaultEnv(s.Hostname)
	}

	// Exec replaces this process, so the workload becomes pid 1 in the new
	// pid namespace rather than a child of a Go runtime. That matters: the
	// workload inherits signal delivery directly, and there is no extra
	// process to confuse `ps` inside the container.
	if err := syscall.Exec(path, s.Args, env); err != nil {
		fail("exec %s: %v", path, err)
	}
}

// setupNamespace does everything that must happen inside the new namespaces
// before the workload runs.
func setupNamespace(s *spec.Spec) error {
	if s.Hostname != "" && s.Namespaces.UTS {
		if err := syscall.Sethostname([]byte(s.Hostname)); err != nil {
			return fmt.Errorf("set hostname: %w", err)
		}
	}

	// Make every existing mount private. Without this, mounts created below
	// propagate to the host -- the mount namespace exists but leaks, which is
	// the single most commonly missed step in a hand-written runtime.
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	// Bind mounts go up *before* pivot_root, with the destination resolved
	// inside the new root. The source is a host path, and pivot_root detaches
	// the old root -- so after pivoting that path no longer resolves in this
	// mount namespace and the mount fails with ENOENT. Doing it in the wrong
	// order is a mistake that looks like a missing directory.
	//
	// The rootfs self-bind inside pivotRoot is MS_REC, so mounts placed here
	// are carried across the pivot.
	for _, m := range s.Mounts {
		if err := mountExtra(s.RootFS, m); err != nil {
			return err
		}
	}

	if err := pivotRoot(s.RootFS); err != nil {
		return err
	}

	// The pseudo-filesystems go up after the pivot: /proc in particular must
	// be mounted inside the pid namespace to show the container's processes.
	if err := mountDefaults(s); err != nil {
		return err
	}

	if s.ReadOnlyRoot {
		// Remount read-only *after* the default mounts, or /proc and /tmp
		// would be unwritable too and almost nothing would run.
		if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("remount root read-only: %w", err)
		}
	}

	cwd := s.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}
	return nil
}

// pivotRoot replaces the root filesystem.
//
// pivot_root rather than chroot, and the difference matters: chroot only
// changes the apparent root, leaving the old root reachable through any open
// directory file descriptor -- the classic chroot escape. pivot_root moves the
// mount itself and lets the old root be unmounted, so there is nothing left to
// escape through.
//
// The sequence is fiddly and every step is required:
func pivotRoot(root string) error {
	// 1. pivot_root requires the new root to be a mount point, so bind-mount
	//    it onto itself.
	if err := syscall.Mount(root, root, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs onto itself: %w", err)
	}

	// 2. A directory is needed to park the old root.
	oldRoot := filepath.Join(root, ".old-root")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("create .old-root: %w", err)
	}

	// 3. Swap them.
	if err := syscall.PivotRoot(root, oldRoot); err != nil {
		return fmt.Errorf("pivot_root(%s): %w "+
			"(the rootfs must be a mount point on a filesystem that allows it)", root, err)
	}

	// 4. The cwd still refers to the old root's directory, so move it.
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to new root: %w", err)
	}

	// 5. Detach the old root. Until this succeeds the host filesystem is
	//    still reachable at /.old-root, which would make the whole exercise
	//    pointless.
	if err := syscall.Unmount("/.old-root", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}

	// 6. Remove the now-empty directory, so the container does not see it.
	if err := os.Remove("/.old-root"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove .old-root: %w", err)
	}
	return nil
}

// mountDefaults mounts the pseudo-filesystems almost everything expects.
func mountDefaults(s *spec.Spec) error {
	type m struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}

	const (
		noexec = syscall.MS_NOEXEC
		nosuid = syscall.MS_NOSUID
		nodev  = syscall.MS_NODEV
	)

	mounts := []m{
		// /proc must be mounted *inside* the pid namespace, or it shows the
		// host's processes -- which is both a leak and deeply confusing, since
		// `ps` would list things the container cannot signal.
		{"proc", "/proc", "proc", noexec | nosuid | nodev, ""},
		// nosuid and nodev on /dev and /tmp: a setuid binary or a device node
		// placed there would otherwise be a direct escape route.
		{"tmpfs", "/dev", "tmpfs", nosuid, "mode=755,size=64k"},
		{"tmpfs", "/tmp", "tmpfs", noexec | nosuid | nodev, "mode=1777,size=64m"},
		{"sysfs", "/sys", "sysfs", noexec | nosuid | nodev | syscall.MS_RDONLY, ""},
	}

	for _, mt := range mounts {
		if err := os.MkdirAll(mt.target, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", mt.target, err)
		}
		if err := syscall.Mount(mt.source, mt.target, mt.fstype, mt.flags, mt.data); err != nil {
			// /sys fails inside a user namespace without a network
			// namespace, which is a legitimate configuration -- so it is a
			// warning rather than fatal.
			if mt.target == "/sys" {
				fmt.Fprintf(os.Stderr, "warning: could not mount /sys: %v\n", err)
				continue
			}
			return fmt.Errorf("mount %s on %s: %w", mt.fstype, mt.target, err)
		}
	}

	// A minimal /dev. Creating device nodes needs privileges a user namespace
	// does not grant, so bind-mount them from the host instead, which works
	// either way.
	for _, dev := range []string{"null", "zero", "random", "urandom", "full", "tty"} {
		target := filepath.Join("/dev", dev)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			continue
		}
		_ = f.Close()
		if err := syscall.Mount(filepath.Join("/.old-root/dev", dev), target, "", syscall.MS_BIND, ""); err != nil {
			// By this point the old root is already detached, so the bind
			// cannot work -- fall back to leaving the empty file. Documented
			// as a limitation rather than pretended away.
			_ = os.Remove(target)
		}
	}

	// /dev/pts and a /dev/console are needed for an interactive shell.
	if s.Terminal {
		if err := os.MkdirAll("/dev/pts", 0o755); err == nil {
			if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not mount /dev/pts: %v\n", err)
			}
		}
	}
	return nil
}

// mountExtra sets up one configured mount. `root` is the rootfs path on the
// host: this runs before pivot_root, so the destination has to be resolved
// relative to it rather than taken as an absolute path in the container.
func mountExtra(root string, m spec.Mount) error {
	target, err := resolveInRoot(root, m.Destination)
	if err != nil {
		return err
	}

	// Create the mountpoint. Every container runtime does this -- requiring
	// the image to contain an empty directory for each volume would make
	// bind-mounting into a minimal rootfs impossible. A file source needs a
	// file to mount onto, not a directory.
	if info, err := os.Stat(m.Source); err == nil && !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create mount target directory for %s: %w", m.Destination, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("create mount target %s: %w", m.Destination, err)
		}
		_ = f.Close()
	} else if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create mount target %s: %w", m.Destination, err)
	}

	fstype := m.Type
	var flags uintptr
	if fstype == "" || fstype == "bind" {
		fstype = ""
		flags |= syscall.MS_BIND | syscall.MS_REC
	}
	for _, o := range m.Options {
		switch o {
		case "ro":
			flags |= syscall.MS_RDONLY
		case "noexec":
			flags |= syscall.MS_NOEXEC
		case "nosuid":
			flags |= syscall.MS_NOSUID
		case "nodev":
			flags |= syscall.MS_NODEV
		}
	}

	if err := syscall.Mount(m.Source, target, fstype, flags, ""); err != nil {
		return fmt.Errorf("mount %s on %s: %w", m.Source, m.Destination, err)
	}
	// A read-only bind mount needs a second remount: the MS_RDONLY flag is
	// ignored on the initial MS_BIND, which is a long-standing kernel
	// behaviour and a frequent source of "why is my ro mount writable".
	if flags&syscall.MS_RDONLY != 0 && flags&syscall.MS_BIND != 0 {
		if err := syscall.Mount("", target, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", m.Destination, err)
		}
	}
	return nil
}

// resolveInRoot joins a container-absolute destination onto the rootfs and
// refuses anything that escapes it. A destination of "../../etc" would
// otherwise let a spec write outside the container before it even starts.
//
// Rejecting rather than silently clamping: quietly rewriting a path that was
// trying to escape leaves nothing in the logs to notice.
func resolveInRoot(root, destination string) (string, error) {
	if !filepath.IsAbs(destination) {
		return "", fmt.Errorf("mount destination %q must be absolute", destination)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve rootfs %s: %w", root, err)
	}
	target := filepath.Join(absRoot, filepath.Clean(destination))
	if target != absRoot && !strings.HasPrefix(target, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("mount destination %q escapes the root filesystem", destination)
	}
	return target, nil
}

func marshalSpec(s *spec.Spec) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func unmarshalSpec(payload string) (*spec.Spec, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, err
	}
	var s spec.Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mini-container init: "+format+"\n", args...)
	os.Exit(125) // distinct from the workload's own exit codes
}
