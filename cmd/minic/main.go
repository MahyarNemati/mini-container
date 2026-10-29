// Command minic runs a process in Linux namespaces with cgroup limits.
//
//	sudo minic run --image alpine -- /bin/sh
//	sudo minic run --rootfs ./rootfs --memory 64m --cpu 500m -- /bin/echo hi
//	minic pull alpine ./rootfs
//	minic info
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MahyarNemati/mini-container/internal/cgroups"
	"github.com/MahyarNemati/mini-container/internal/container"
	"github.com/MahyarNemati/mini-container/internal/image"
	"github.com/MahyarNemati/mini-container/internal/spec"
)

var version = "dev"

func main() {
	// This must be the very first thing: when the runtime re-executes itself
	// to enter new namespaces, the child is this same binary and has to take
	// the init path before any flag parsing or logging happens.
	if len(os.Args) > 1 && os.Args[1] == container.InitArg {
		container.Init()
		return // unreachable: Init execs or exits
	}

	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, container.ErrUnsupportedPlatform) {
			fmt.Fprintf(os.Stderr, "minic: %v\n\n%s", err, platformHelp)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "minic: %v\n", err)
		os.Exit(1)
	}
}

const platformHelp = `Containers are a Linux kernel feature: namespaces, cgroups and overlayfs have
no equivalent on macOS or Windows. To try this out:

  # In Docker (ironic, but it is a real Linux kernel)
  docker run --rm -it --privileged -v "$PWD:/src" -w /src golang:1.25 \
    sh -c 'go build -o minic ./cmd/minic && ./minic info'

  # Or any Linux VM, as root.
`

const usage = `minic - a small container runtime

usage:
  minic run   [flags] -- <command> [args...]   run a container
  minic pull  <image> <dir>                    download and unpack an image
  minic info                                   report what this host supports
  minic version

run flags:
  --image <ref>        image to use, e.g. alpine or ghcr.io/user/img:tag
  --rootfs <dir>       use an existing root filesystem instead of an image
  --id <name>          container id (default: generated)
  --hostname <name>    hostname inside the container (default: the id)
  --memory <size>      memory limit, e.g. 64m or 1g
  --cpu <millis>       CPU limit, e.g. 500m for half a core
  --pids <n>           maximum processes (default 128; 0 for unlimited)
  --timeout <dur>      kill the container after this long
  --read-only          mount the root filesystem read-only
  --no-net             unshare the network namespace (loopback only)
  --insecure-registry  pull over plain HTTP (for a local registry)
  --userns             use a user namespace, mapping root to the current user
  --env KEY=VALUE      set an environment variable (repeatable)
  --volume SRC:DST[:ro]  bind-mount a host path (repeatable)
  --state <dir>        where to keep overlay layers (default /var/lib/mini-container)
  --cgroup-root <dir>  cgroup v2 mount point (default /sys/fs/cgroup)
  --quiet              suppress runtime diagnostics

examples:
  sudo minic run --image alpine -- /bin/sh
  sudo minic run --image alpine --memory 32m --pids 16 -- /bin/sh -c 'echo hi'
  sudo minic run --rootfs ./rootfs --no-net --read-only -- /bin/ls /
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}

	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "pull":
		return cmdPull(args[1:])
	case "info":
		return cmdInfo()
	case "version", "--version", "-v":
		fmt.Println("minic", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type runFlags struct {
	image, rootfs, id, hostname string
	memory, cpu                 string
	pids                        int64
	timeout                     time.Duration
	readOnly, noNet, userns     bool
	insecure                    bool
	env, volumes                []string
	stateDir, cgroupRoot        string
	quiet                       bool
	argv                        []string
}

// parseRunFlags is hand-rolled rather than using the flag package, because
// the `--` separator before the container's command has to be respected
// exactly: everything after it belongs to the container, including things
// that look like our own flags.
func parseRunFlags(args []string) (*runFlags, error) {
	f := &runFlags{pids: 128}

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unexpected argument %q (use -- before the container command)", a)
		}

		name := strings.TrimPrefix(a, "--")
		var value string
		if eq := strings.Index(name, "="); eq >= 0 {
			name, value = name[:eq], name[eq+1:]
		}

		needsValue := func() (string, error) {
			if value != "" {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("--%s needs a value", name)
			}
			i++
			return args[i], nil
		}

		var err error
		switch name {
		case "image":
			f.image, err = needsValue()
		case "rootfs":
			f.rootfs, err = needsValue()
		case "id":
			f.id, err = needsValue()
		case "hostname":
			f.hostname, err = needsValue()
		case "memory":
			f.memory, err = needsValue()
		case "cpu":
			f.cpu, err = needsValue()
		case "pids":
			var v string
			if v, err = needsValue(); err == nil {
				f.pids, err = strconv.ParseInt(v, 10, 64)
			}
		case "timeout":
			var v string
			if v, err = needsValue(); err == nil {
				f.timeout, err = time.ParseDuration(v)
			}
		case "state":
			f.stateDir, err = needsValue()
		case "cgroup-root":
			f.cgroupRoot, err = needsValue()
		case "env":
			var v string
			if v, err = needsValue(); err == nil {
				if !strings.Contains(v, "=") {
					err = fmt.Errorf("--env needs KEY=VALUE, got %q", v)
				} else {
					f.env = append(f.env, v)
				}
			}
		case "volume", "v":
			var v string
			if v, err = needsValue(); err == nil {
				f.volumes = append(f.volumes, v)
			}
		case "insecure-registry":
			f.insecure = true
		case "read-only":
			f.readOnly = true
		case "no-net":
			f.noNet = true
		case "userns":
			f.userns = true
		case "quiet":
			f.quiet = true
		default:
			err = fmt.Errorf("unknown flag --%s", name)
		}
		if err != nil {
			return nil, err
		}
	}

	f.argv = args[i:]
	if len(f.argv) == 0 {
		return nil, errors.New("no command given; put it after --")
	}
	return f, nil
}

func cmdRun(args []string) error {
	f, err := parseRunFlags(args)
	if err != nil {
		fmt.Fprint(os.Stderr, usage)
		return err
	}

	if err := container.Preflight(f.cgroupRoot); err != nil {
		return err
	}

	if f.id == "" {
		f.id = generateID()
	}
	if f.hostname == "" {
		f.hostname = f.id
	}

	s := &spec.Spec{
		ID:           f.id,
		Args:         f.argv,
		Hostname:     f.hostname,
		Namespaces:   spec.DefaultNamespaces(),
		ReadOnlyRoot: f.readOnly,
		Timeout:      f.timeout,
		Terminal:     isTerminal(os.Stdin),
		Env:          f.env,
	}
	s.Namespaces.Network = f.noNet
	if f.userns {
		s.Namespaces.User = true
		s.HostUID, s.HostGID = os.Getuid(), os.Getgid()
	}

	s.Resources.PidsMax = f.pids
	if f.memory != "" {
		if s.Resources.MemoryBytes, err = parseSize(f.memory); err != nil {
			return err
		}
		// Pin swap to zero alongside a memory limit: otherwise hitting the
		// limit degrades into swap thrashing instead of a clean OOM kill,
		// which is slower and much harder to diagnose.
		s.Resources.MemorySwapBytes = 0
	}
	if f.cpu != "" {
		if s.Resources.CPUMillis, err = parseMillis(f.cpu); err != nil {
			return err
		}
	}

	for _, v := range f.volumes {
		m, err := parseVolume(v)
		if err != nil {
			return err
		}
		s.Mounts = append(s.Mounts, m)
	}

	stateDir := f.stateDir
	if stateDir == "" {
		stateDir = "/var/lib/mini-container"
	}

	switch {
	case f.image != "":
		ref, err := image.ParseReference(f.image)
		if err != nil {
			return err
		}
		cacheDir := filepath.Join(stateDir, "images", sanitize(ref.String()))
		puller := image.NewPuller()
		puller.Insecure = f.insecure
		if !f.quiet {
			puller.Progress = func(msg string) { fmt.Fprintf(os.Stderr, "pull: %s\n", msg) }
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := puller.Pull(ctx, ref, cacheDir); err != nil {
			return err
		}
		s.Image = cacheDir
	case f.rootfs != "":
		s.RootFS = f.rootfs
	default:
		return errors.New("one of --image or --rootfs is required")
	}

	result, err := container.Run(container.Config{
		Spec: s, CgroupRoot: f.cgroupRoot, StateDir: stateDir,
	})
	if err != nil {
		return err
	}

	if !f.quiet {
		printSummary(result)
	}
	// Propagate the container's exit code, so minic composes in a script the
	// way any other command would.
	os.Exit(result.ExitCode)
	return nil
}

func printSummary(r *container.Result) {
	fmt.Fprintf(os.Stderr, "\n--- container finished ---\n")
	fmt.Fprintf(os.Stderr, "exit code:   %d", r.ExitCode)
	if r.Signal != 0 {
		fmt.Fprintf(os.Stderr, " (killed by signal %d)", r.Signal)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "duration:    %s\n", r.Duration.Round(time.Millisecond))
	if r.TimedOut {
		fmt.Fprintf(os.Stderr, "timed out:   yes\n")
	}
	// Distinguishing an OOM kill from an ordinary SIGKILL is only possible
	// from the cgroup counter, and it is the first thing you want to know.
	if r.OOMKilled {
		fmt.Fprintf(os.Stderr, "OOM killed:  yes (%d kill(s)) -- raise --memory\n", r.Stats.OOMKills)
	}
	if r.Stats.MemoryPeak > 0 {
		fmt.Fprintf(os.Stderr, "peak memory: %s\n", humanBytes(r.Stats.MemoryPeak))
	}
	if r.Stats.CPUUsage > 0 {
		fmt.Fprintf(os.Stderr, "cpu time:    %s\n", r.Stats.CPUUsage.Round(time.Millisecond))
	}
	if r.Stats.Throttled > 0 {
		fmt.Fprintf(os.Stderr, "throttled:   %d time(s) -- the CPU limit was reached\n", r.Stats.Throttled)
	}
}

func cmdPull(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: minic pull <image> <dir>")
	}
	ref, err := image.ParseReference(args[0])
	if err != nil {
		return err
	}
	puller := image.NewPuller()
	puller.Progress = func(msg string) { fmt.Fprintf(os.Stderr, "pull: %s\n", msg) }

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "pulling %s\n", ref)
	if err := puller.Pull(ctx, ref, args[1]); err != nil {
		return err
	}
	fmt.Printf("unpacked %s into %s\n", ref, args[1])
	return nil
}

// cmdInfo reports what the host supports, so a failure to run can be
// diagnosed before it happens rather than from a bare EPERM.
func cmdInfo() error {
	fmt.Printf("minic %s\n\n", version)

	fmt.Printf("platform:        %s\n", runtimeDescription())
	fmt.Printf("euid:            %d", os.Geteuid())
	if os.Geteuid() != 0 {
		fmt.Printf("  (not root: use sudo, or --userns)")
	}
	fmt.Println()

	root := cgroups.DefaultRoot
	if err := cgroups.Available(root); err != nil {
		fmt.Printf("cgroup v2:       no (%v)\n", err)
	} else {
		fmt.Printf("cgroup v2:       yes at %s\n", root)
		if controllers, err := cgroups.Controllers(root); err == nil {
			fmt.Printf("controllers:     %s\n", strings.Join(controllers, " "))
			// Say which of the ones we need are missing, rather than leaving
			// the reader to compare two lists.
			have := map[string]bool{}
			for _, c := range controllers {
				have[c] = true
			}
			var missing []string
			for _, want := range []string{"memory", "cpu", "pids"} {
				if !have[want] {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				fmt.Printf("missing:         %s (limits for these will not apply)\n", strings.Join(missing, " "))
			}
		}
	}

	if err := overlaySupported(); err != nil {
		fmt.Printf("overlayfs:       no (%v)\n", err)
	} else {
		fmt.Printf("overlayfs:       yes\n")
	}

	if err := container.Preflight(""); err != nil {
		fmt.Printf("\nready:           no -- %v\n", err)
		return nil
	}
	fmt.Printf("\nready:           yes\n")
	return nil
}

// ---------- parsing helpers ----------

// parseSize parses a byte size with an optional k/m/g suffix.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "kb"):
		mult, s = 1024, strings.TrimRight(s, "kb")
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "mb"):
		mult, s = 1024*1024, strings.TrimRight(s, "mb")
	case strings.HasSuffix(s, "g"), strings.HasSuffix(s, "gb"):
		mult, s = 1024*1024*1024, strings.TrimRight(s, "gb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimRight(s, "b")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (try 64m or 1g)", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative")
	}
	return n * mult, nil
}

// parseMillis parses a CPU quantity: "500m" or "1.5".
func parseMillis(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid cpu %q (try 500m)", s)
		}
		return n, nil
	}
	cores, err := strconv.ParseFloat(s, 64)
	if err != nil || cores < 0 {
		return 0, fmt.Errorf("invalid cpu %q (try 500m or 1.5)", s)
	}
	return int64(cores * 1000), nil
}

// parseVolume parses SRC:DST[:ro].
func parseVolume(v string) (spec.Mount, error) {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return spec.Mount{}, fmt.Errorf("invalid --volume %q (want SRC:DST or SRC:DST:ro)", v)
	}
	src, err := filepath.Abs(parts[0])
	if err != nil {
		return spec.Mount{}, err
	}
	if _, err := os.Stat(src); err != nil {
		return spec.Mount{}, fmt.Errorf("--volume source %s: %w", src, err)
	}
	m := spec.Mount{Source: src, Destination: parts[1], Type: "bind"}
	if len(parts) == 3 {
		if parts[2] != "ro" {
			return spec.Mount{}, fmt.Errorf("invalid --volume option %q (only 'ro' is supported)", parts[2])
		}
		m.Options = []string{"ro"}
	}
	return m, nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func generateID() string {
	return "c" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
