# mini-container

[![CI](https://github.com/MahyarNemati/mini-container/actions/workflows/ci.yml/badge.svg)](https://github.com/MahyarNemati/mini-container/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/MahyarNemati/mini-container)](https://goreportcard.com/report/github.com/MahyarNemati/mini-container)

A container runtime in ~2,000 lines of Go: Linux namespaces, cgroup v2 limits,
an overlayfs root filesystem, and an OCI image puller. No runc, no libcontainer,
no container library of any kind — just syscalls.

```bash
sudo minic run --image alpine -- /bin/sh
sudo minic run --rootfs ./rootfs --memory 32m --cpu 500m --pids 16 -- /bin/sh
minic info      # what does this host actually support?
```

**Linux only**, necessarily — namespaces and cgroups are kernel features with
no equivalent elsewhere. On a Mac:

```bash
make demo   # runs the whole thing inside a privileged Docker container
```

## What a container actually is

A container is not a thing the kernel has. It is a process with five or six
kernel features applied to it, and `minic run` is a demonstration of exactly
which ones:

| Feature | What it hides | Without it |
|---|---|---|
| **mount namespace** | the filesystem tree | the process sees the host's files; nothing else matters |
| **PID namespace** | other processes | `ps` lists the host, and pid 1 duties fall to nobody |
| **UTS namespace** | hostname | `hostname` lies |
| **IPC namespace** | SysV shared memory | two containers collide on a well-known key |
| **network namespace** | interfaces and routes | the container shares the host's network stack |
| **user namespace** | uid/gid mapping | root inside is root outside |
| **cgroup v2** | nothing — it *limits* | one container can exhaust the host |
| **overlayfs** | nothing — it *shares* | every container copies the whole image |

## The three things that are genuinely hard

### 1. You cannot unshare namespaces from Go

This is the first wall everyone hits. `clone(CLONE_NEWNS|CLONE_NEWPID)` applies
to the **calling thread**, and the Go runtime is always multi-threaded with no
way to control which thread runs your code or what the others are doing.
Calling `unshare` from a goroutine gives you a namespace that applies to one
arbitrary thread.

The solution — the same one runc uses — is to **re-execute the runtime binary**:

```
parent                                    child (/proc/self/exe --init)
  │                                         │
  ├─ exec.Command("/proc/self/exe", init)   │
  │    SysProcAttr.Cloneflags = CLONE_NEW*  │
  │    ...kernel clones ───────────────────▶│  starts single-threaded,
  │                                         │  already in new namespaces
  ├─ write child pid to cgroup.procs        │
  │                                         ├─ mount / private
  │                                         ├─ pivot_root
  │                                         ├─ mount /proc, /dev, /tmp
  │                                         └─ execve(workload)
  └─ wait ─────────────────────────────────▶
```

That is why `cmd/minic/main.go` checks for a sentinel argument before anything
else, and why `container.Init()` never returns.

The ordering in the parent is not arbitrary either. The pid is written to
`cgroup.procs` **after** clone and **before** the workload execs — the only
window in which limits can be applied. Any earlier there is no pid; any later
and the workload has already run unlimited, which for a fork bomb is long
enough to matter.

### 2. pivot_root, not chroot

`chroot` only changes the apparent root. The old root stays reachable through
any open directory file descriptor, which is the classic chroot escape.
`pivot_root` moves the mount itself so the old root can be unmounted and there
is nothing left to escape through.

The sequence is fiddly and every step is load-bearing:

```go
mount(root, root, MS_BIND|MS_REC)   // pivot_root needs a mount point
mkdir(root + "/.old-root")          // somewhere to park the old root
pivot_root(root, oldRoot)           // swap them
chdir("/")                          // cwd still points into the old root
umount("/.old-root", MNT_DETACH)    // ← until this, the host is reachable
rmdir("/.old-root")                 // don't leave it visible
```

Before any of that: `mount("", "/", MS_PRIVATE|MS_REC)`. Without it every
mount the container makes **propagates back to the host** — the namespace
exists but leaks. It is the single most commonly forgotten line in a
hand-written runtime, and `TestMountNamespaceDoesNotLeakToTheHost` is there
because of it.

`/proc` must be mounted **inside** the pid namespace, or it shows the host's
processes: both a leak and deeply confusing, since `ps` lists things the
container cannot signal.

### 3. cgroup v2 controllers must be delegated

The step that silently does nothing. A controller appearing in
`cgroup.controllers` means the **parent** has it; a child cgroup cannot use it
until the parent delegates it through `cgroup.subtree_control`:

```
echo "+memory +cpu +pids" > /sys/fs/cgroup/cgroup.subtree_control
```

Skip that and `memory.max` does not exist in the new group, the write fails,
and the limit the operator set is simply absent. Related: this code opens
control files **without** `O_CREATE`, because `os.WriteFile` would happily
create a regular file that the kernel ignores — a limit you believe is applied
and is not. A test asserts the error is reported rather than swallowed.

**Distinguishing an OOM kill from an ordinary failure** needs the cgroup too:
the process just sees SIGKILL either way. `memory.events`'s `oom_kill` counter
is the only reliable signal, which is why `Result.OOMKilled` exists and why
`minic` prints "OOM killed: yes — raise --memory" instead of leaving you to
guess.

## overlayfs, and two behaviours that surprise people

An overlay is four directories: the image (`lowerdir`, read-only and shared
between containers), this container's writes (`upperdir`), kernel scratch space
(`workdir`), and the union the container sees (`merged`). That is how a
container starts instantly from a 200 MB image without copying it.

Two consequences worth knowing before you debug them:

- **Copy-up.** Writing one byte to a file that exists only in the lower layer
  copies the *entire file* into the upper layer first. A container appending to
  a 2 GB log file from the image pauses for seconds. Image layers should not
  contain large mutable files.
- **Whiteouts.** Deleting a lower-layer file creates a character device with
  device number 0 in the upper layer. So `rm` inside a container does not free
  space — it uses a little.

`minic` reports how many paths a container wrote to its upper layer, which is
the same information `docker diff` gives you.

## The image puller

`minic pull alpine` speaks the registry API directly, and the flow has one step
naive implementations skip:

1. `GET /v2/` → the registry answers **401** with a `WWW-Authenticate` header
   naming a token service. Anonymous pulls still need a token.
2. Fetch the token with the right scope.
3. `GET /v2/<name>/manifests/<ref>` → this may be an **index** (a list of
   per-platform manifests) rather than a manifest, in which case you pick your
   architecture and fetch again. Implementations that assume a manifest work on
   amd64 and fail confusingly on arm64.
4. Fetch each layer and extract it over the previous ones.

Extraction handles what the layer format actually contains:

- **`.wh.<name>` whiteouts** mean "this file was deleted in this layer" and
  must be *interpreted*, not extracted as a file.
- **`.wh..wh..opq`** means "ignore everything from lower layers in this
  directory".
- **Digests are verified while streaming**, not after buffering — a layer can
  be hundreds of megabytes, and the digest is the only thing making this a
  content-addressed fetch rather than blind trust in the network.
- **Traversing entries are refused.** A layer containing `../../etc/passwd` is
  a real attack. Cleaning it to `/etc/passwd` would also be safe, but silently
  rewriting a malicious path leaves no trace, so it is rejected instead.

Pulls are cached by digest, because the alternative is downloading 50 MB every
time a container starts.

## Usage

```
minic run [flags] -- <command> [args...]

  --image <ref>          pull and use an image (alpine, ghcr.io/user/img:tag)
  --rootfs <dir>         use an existing root filesystem
  --memory <size>        64m, 1g
  --cpu <millis>         500m for half a core, or 1.5
  --pids <n>             default 128; the cheapest fork-bomb protection
  --timeout <dur>        kill the container after this long
  --read-only            read-only root (/tmp and /proc stay writable)
  --no-net               unshare the network namespace
  --userns               map container root to the current user
  --volume SRC:DST[:ro]  bind-mount a host path
  --env KEY=VALUE        the host environment is never inherited
  --insecure-registry    pull over plain HTTP, for a local registry

minic pull <image> <dir>
minic info
```

`minic info` is worth running first — it reports euid, whether cgroup v2 is
mounted, which controllers are available, **which of the ones we need are
missing**, and whether overlayfs exists. A bare `EPERM` from a syscall is a
miserable way to learn any of that.

## Testing

```bash
make test               # portable tests: run on macOS too
make cross              # both build paths still compile
make test-integration   # real containers; Linux and root only
make demo               # the whole thing inside privileged Docker
```

The split matters and is deliberate. The syscalls are behind `//go:build linux`
with stubs elsewhere, so **the logic that is not a syscall is testable
anywhere** — spec validation, cgroup file handling against a temporary
directory, overlay option formatting, registry reference parsing, and layer
extraction including whiteouts and traversal attempts. That is where most of
the bugs actually live, and CI runs those on macOS as well as Linux to keep the
stubs honest.

The things that cannot be faked — pid 1, pivot_root, OOM kills, mount
propagation — run as real containers on a Linux runner, behind a build tag.
Mocking them would have tested nothing.

CI also pulls `alpine:3.20` from Docker Hub and runs it, because the registry
auth challenge and index resolution only break against a real registry.

## Limitations

This is a teaching implementation and the gaps are the interesting part:

- **No seccomp, no capability dropping, no AppArmor/SELinux.** A real runtime
  drops ~30 capabilities and installs a seccomp filter blocking ~300 syscalls.
  Without those, a root process inside the container is much closer to root
  outside than the namespaces alone suggest. **Do not run untrusted code in
  this.**
- **`--no-net` gives you loopback and nothing else.** Real connectivity needs
  a veth pair, a bridge, and iptables rules on the host — a substantial project
  of its own.
- **No `/dev` device nodes.** `mknod` needs privileges a user namespace does
  not grant, and bind-mounting them from the host has to happen before the old
  root is detached; the current ordering does not, so `/dev` is a near-empty
  tmpfs. Most programs cope; anything needing `/dev/null` as a real device
  does not.
- **No image config.** The manifest's config blob (entrypoint, env, working
  directory, user) is fetched but not applied, so `--image alpine` still needs
  an explicit command.
- **No lifecycle.** No `start`/`stop`/`exec`/`ps`, no state persisted between
  invocations, no detached containers. One process, foreground, until it exits.
- **Not OCI compliant.** The spec is a deliberate subset; this will not work
  with containerd or Kubernetes.
- **No rootless support in practice.** `--userns` maps root, but without
  newuidmap/newgidmap and a subuid range the mappings are limited to a single
  id, which breaks most images.

## License

MIT — see [LICENSE](LICENSE).
