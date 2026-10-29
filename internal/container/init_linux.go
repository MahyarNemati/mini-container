//go:build linux

package container

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// /dev and its device nodes go up before the pivot, for the same reason
	// the bind mounts below do: the nodes are bind-mounted from the host, and
	// after pivot_root the host's /dev is no longer reachable.
	if err := mountDev(s.RootFS); err != nil {
		return err
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
		// nosuid and nodev on /tmp: a setuid binary or a device node placed
		// there would otherwise be a direct escape route. /dev is mounted
		// before the pivot instead -- see mountDev.
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

// devNodes are the character devices essentially every program expects. They
// are bind-mounted one at a time from the host rather than created with
// mknod(2), which needs CAP_MKNOD in the initial user namespace -- precisely
// what a rootless container does not have.
var devNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// mountDev puts a tmpfs on the container's /dev and binds the standard device
// nodes into it.
//
// This has to run before pivot_root: the sources are host paths, and the
// pivot detaches the old root. Doing it afterwards is why an earlier version
// of this runtime had no /dev at all, so `dd if=/dev/zero` failed with "No
// such file or directory" and the gap was written up as a limitation instead
// of fixed.
//
// nosuid and nodev on the tmpfs itself: with nodev the kernel ignores device
// nodes on that filesystem, so the bind mounts are the only devices that
// work and a node the container creates for itself does nothing.
func mountDev(root string) error {
	target := filepath.Join(root, "dev")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create /dev: %w", err)
	}
	if err := syscall.Mount("tmpfs", target, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV, "mode=755,size=256k"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}

	for _, name := range devNodes {
		source := filepath.Join("/dev", name)
		if _, err := os.Stat(source); err != nil {
			// A host without the node is not an error: /dev/tty is absent in
			// some build environments and nothing here needs it.
			continue
		}
		dest := filepath.Join(target, name)
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return fmt.Errorf("create /dev/%s: %w", name, err)
		}
		_ = f.Close()
		if err := syscall.Mount(source, dest, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind /dev/%s: %w", name, err)
		}
	}

	// The three standard streams, as the symlinks every shell expects.
	for link, dest := range map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
	} {
		// Pointing into /proc, which is mounted after the pivot -- a symlink
		// is resolved when it is used, so the order does not matter here.
		if err := os.Symlink(dest, filepath.Join(target, link)); err != nil && !os.IsExist(err) {
			return fmt.Errorf("link /dev/%s: %w", link, err)
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
