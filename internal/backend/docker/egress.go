package docker

// network.mode allowlist:
//
//	sandbox ──(internal network "ephemeris-<session>", no route out)──▶ proxy "egress:3128"
//	proxy ──(engine's default bridge)──▶ internet, CONNECT to allowed hosts only
//
// the sandbox has exactly one interface, on a network the engine marks
// Internal, so a client that ignores HTTPS_PROXY has nowhere to go. the proxy
// sidecar is the only container on both networks, and it runs our own
// netpolicy code (`ephemeris __proxy`), which also refuses private addresses
// after DNS so allowed.com -> 10.0.0.1 cannot reach your LAN

import (
	"bufio"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/netpolicy"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

const (
	// LabelRole marks infrastructure containers, sandboxes carry no role label
	LabelRole = "dev.ephemeris.role"
	RoleProxy = "proxy"

	// EnvProxyImage names an image containing a static linux ephemeris binary
	// at /ephemeris. needed wherever this binary cannot be bind-mounted
	EnvProxyImage = "EPHEMERIS_PROXY_IMAGE"

	proxyBaseImage = "docker.io/library/alpine:3.20" // tiny; only supplies a rootfs
	proxyBinPath   = "/ephemeris"
	proxyAlias     = "egress"
	// the default network every engine has. Docker calls it "bridge";
	// Podman's compat API maps "bridge" onto its default network too
	outsideNetwork = "bridge"
)

func networkName(session string) string { return "ephemeris-" + session }
func proxyName(session string) string   { return "ephemeris-" + session + "-proxy" }

// proxyEnv points every common HTTP client at the sidecar. both cases:
// curl only reads lowercase http_proxy, Go and Python read either
func proxyEnv() []string {
	u := "http://" + proxyAlias + ":" + netpolicy.ProxyPort
	return []string{
		"HTTPS_PROXY=" + u, "HTTP_PROXY=" + u, "https_proxy=" + u, "http_proxy=" + u,
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
}

// proxyLaunch is how the proxy container gets the ephemeris binary:
// either bin is bind-mounted into proxyBaseImage, or image already has it
type proxyLaunch struct {
	image string
	bin   string // host path; "" when image ships the binary
}

func (b *Backend) resolveProxy(ctx context.Context) (proxyLaunch, error) {
	if img := os.Getenv(EnvProxyImage); img != "" {
		return proxyLaunch{image: img}, nil
	}
	var why string
	switch {
	case runtime.GOOS != "linux":
		why = "the engine runs Linux in a VM on " + runtime.GOOS + " and cannot execute this " + runtime.GOOS + " binary"
	case !strings.HasPrefix(b.cli.DaemonHost(), "unix://"):
		why = "the engine at " + b.cli.DaemonHost() + " is remote and cannot see this machine's files"
	default:
		v, err := b.cli.ServerVersion(ctx)
		if err != nil {
			return proxyLaunch{}, fmt.Errorf("engine version: %w", err)
		}
		if v.Arch != "" && v.Arch != runtime.GOARCH {
			why = "the engine is " + v.Arch + " but this binary is " + runtime.GOARCH
		}
	}
	if why != "" {
		return proxyLaunch{}, proxyImageHelp(why)
	}

	bin := b.ProxyBinary
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return proxyLaunch{}, fmt.Errorf("locate ephemeris binary for the egress proxy: %w", err)
		}
		bin = exe
	}
	bin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return proxyLaunch{}, fmt.Errorf("egress proxy binary: %w", err)
	}
	if err := checkMountableBinary(bin); err != nil {
		return proxyLaunch{}, err
	}
	return proxyLaunch{image: proxyBaseImage, bin: bin}, nil
}

func proxyImageHelp(why string) error {
	return fmt.Errorf("network.mode allowlist runs the ephemeris binary in an egress proxy container, but %s.\n"+
		"Build a proxy image for the engine's platform and point %s at it, e.g.:\n"+
		"  CGO_ENABLED=0 GOOS=linux GOARCH=<engine arch> go build -o ephemeris-linux ./cmd/ephemeris\n"+
		"  printf 'FROM scratch\\nCOPY ephemeris-linux %s\\n' | docker build -t localhost/ephemeris-proxy -f - .\n"+
		"  export %s=localhost/ephemeris-proxy",
		why, EnvProxyImage, proxyBinPath, EnvProxyImage)
}

// checkMountableBinary fails early, on the host, for binaries that would only
// fail later inside the container with an opaque "exec format error"
func checkMountableBinary(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("egress proxy binary: %w", err)
	}
	// the proxy runs as nobody, which is "other" to the file's owner
	if st.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("egress proxy binary %s is not executable by other users (mode %s): chmod o+rx it, or set %s", path, st.Mode().Perm(), EnvProxyImage)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("egress proxy binary %s is not a Linux ELF executable (%v); set %s", path, err, EnvProxyImage)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("egress proxy binary %s is dynamically linked and will not run in a minimal container: rebuild with CGO_ENABLED=0 (`make build`), or set %s", path, EnvProxyImage)
		}
	}
	return nil
}

// startEgress creates the internal network and a running, ready proxy on it.
// it may leave partial resources behind on error; Create cleans them up by label
func (b *Backend) startEgress(ctx context.Context, s spec.Spec, labels map[string]string, launch proxyLaunch) error {
	session := labels[backend.LabelSession]
	netName := networkName(session)
	if _, err := b.cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver:   "bridge",
		Internal: true, // no gateway to the outside: the engine drops anything leaving the subnet
		Labels:   labels,
	}); err != nil {
		return fmt.Errorf("create internal network: %w", err)
	}

	plabels := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		plabels[k] = v
	}
	plabels[LabelRole] = RoleProxy

	cfg := &container.Config{
		Image:      launch.image,
		Entrypoint: []string{proxyBinPath},
		Cmd: []string{"__proxy", "--listen", ":" + netpolicy.ProxyPort,
			"--allow", strings.Join(s.Network.Allow, ",")}, // spec forbids commas in entries
		User:     "65534:65534", // nobody: needs no file it does not get handed
		Labels:   plabels,
		Hostname: proxyAlias,
	}
	pids := int64(64)
	hc := &container.HostConfig{
		NetworkMode:    outsideNetwork,
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Resources: container.Resources{
			NanoCPUs:  500_000_000, // half a core
			Memory:    64 << 20,
			PidsLimit: &pids,
		},
	}
	if launch.bin != "" {
		hc.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: launch.bin, Target: proxyBinPath, ReadOnly: true}}
		// SELinux (Fedora/RHEL): container_t may not read user_home_t files, and
		// the usual fix, a :z/:Z relabel, would rewrite the label on the user's
		// own binary (and Z would make it unusable outside this container).
		// instead this one container runs unconfined by SELinux (spc_t). the
		// trade-off is accepted because the proxy runs only the code, as nobody,
		// with no capabilities, a read-only rootfs and no-new-privileges, and it
		// shares no mounts with the sandbox. the untrusted sandbox keeps its full
		// SELinux confinement. engines without SELinux ignore the option.
		// use EPHEMERIS_PROXY_IMAGE to keep the proxy confined as well
		hc.SecurityOpt = append(hc.SecurityOpt, "label=disable")
	}
	resp, err := b.cli.ContainerCreate(ctx, cfg, hc, nil, nil, proxyName(session))
	if err != nil {
		return fmt.Errorf("create egress proxy: %w", err)
	}
	if err := b.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start egress proxy: %w", err)
	}
	if err := b.waitProxyReady(ctx, resp.ID); err != nil {
		return err
	}
	// join the internal network only now, while running. two reasons:
	// older API versions accept one network per create, and Podman builds
	// resolv.conf at start: started on the bridge alone, the proxy keeps the
	// host's resolvers, and connecting later merely prepends the internal
	// network's DNS (which answers NXDOMAIN for outside names; see
	// netpolicy.ResolvConfResolver). connected before start, it gets only that
	if err := b.cli.NetworkConnect(ctx, netName, resp.ID, &network.EndpointSettings{Aliases: []string{proxyAlias}}); err != nil {
		return fmt.Errorf("attach egress proxy to %s: %w", netName, err)
	}
	return nil
}

// waitProxyReady follows the proxy's logs until it says it is listening. if it
// dies first (bad binary, bad flags) the stream ends and we report its output
func (b *Backend) waitProxyReady(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rc, err := b.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return fmt.Errorf("egress proxy logs: %w", err)
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	defer pr.Close() // unblocks the copier if we return early
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		pw.CloseWithError(err)
	}()
	var tail []string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, netpolicy.ReadyMsg) {
			return nil
		}
		tail = append(tail[max(0, len(tail)-4):], line)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("egress proxy not ready after 15s: %w", ctx.Err())
	}
	return fmt.Errorf("egress proxy exited before it was ready: %s", strings.Join(tail, " | "))
}

// sessionFilter matches every resource we made for one session
func sessionFilter(session string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", backend.LabelManaged+"=true"),
		filters.Arg("label", backend.LabelSession+"="+session),
	)
}

// removeSession removes container id (if any) plus every container and
// network labelled with session, then verifies nothing is left. every step
// tolerates "not found", so it is safe to call any number of times
func (b *Backend) removeSession(ctx context.Context, id, session string) error {
	ids := map[string]bool{}
	if id != "" {
		ids[id] = true
	}
	if session != "" {
		cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: sessionFilter(session)})
		if err != nil {
			return fmt.Errorf("list containers of session %s: %w", session, err)
		}
		for _, c := range cs {
			ids[c.ID] = true
		}
	}
	var errs []error
	// containers first: a network with attached endpoints cannot be removed
	for cid := range ids {
		if err := b.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !client.IsErrNotFound(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", short(cid), err))
		}
	}
	if session != "" {
		nets, err := b.cli.NetworkList(ctx, network.ListOptions{Filters: sessionFilter(session)})
		if err != nil {
			errs = append(errs, fmt.Errorf("list networks of session %s: %w", session, err))
		}
		for _, n := range nets {
			if err := b.removeNetwork(ctx, n.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return b.verifyGone(ctx, id, session)
}

func (b *Backend) removeNetwork(ctx context.Context, id string) error {
	var err error
	// an endpoint can linger for a moment after its container is force-removed
	for i := range 5 {
		if err = b.cli.NetworkRemove(ctx, id); err == nil || client.IsErrNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 100 * time.Millisecond):
		}
	}
	return fmt.Errorf("remove network %s: %w", short(id), err)
}

// verifyGone asks the engine, rather than trusting the remove calls
func (b *Backend) verifyGone(ctx context.Context, id, session string) error {
	if id != "" {
		if _, err := b.cli.ContainerInspect(ctx, id); err == nil {
			return fmt.Errorf("container %s still exists after remove", short(id))
		} else if !client.IsErrNotFound(err) {
			return fmt.Errorf("verify removal of %s: %w", short(id), err)
		}
	}
	if session == "" {
		return nil
	}
	cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: sessionFilter(session)})
	if err != nil {
		return fmt.Errorf("verify removal of session %s: %w", session, err)
	}
	if len(cs) > 0 {
		return fmt.Errorf("session %s: %d container(s) still exist after remove", session, len(cs))
	}
	nets, err := b.cli.NetworkList(ctx, network.ListOptions{Filters: sessionFilter(session)})
	if err != nil {
		return fmt.Errorf("verify removal of session %s networks: %w", session, err)
	}
	if len(nets) > 0 {
		return fmt.Errorf("session %s: network %s still exists after remove", session, nets[0].Name)
	}
	return nil
}
