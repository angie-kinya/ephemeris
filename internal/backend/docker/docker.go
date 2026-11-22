// Package docker implements backend.Backend on the Docker Engine API.
// Podman serves the same API, so this works with rootless Podman too:
//
//	systemctl --user enable --now podman.socket
//	export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock
//
// Keep every Docker SDK import inside this package: the SDK moves between
// major versions, and this is the only file that should have to change
package docker

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/go-connections/nat"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

type Backend struct {
	cli *client.Client
	// ProxyBinary is the static linux ephemeris binary bind-mounted into the
	// egress proxy for network.mode allowlist. "" means os.Executable(),
	// which is right for the CLI; tests point it at a freshly built binary
	ProxyBinary string
}

func New(ctx context.Context) (*Backend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx); err != nil {
		cli.Close()
		return nil, fmt.Errorf("cannot reach container engine at %s (start the docker/podman socket or set DOCKER_HOST): %w", cli.DaemonHost(), err)
	}
	return &Backend{cli: cli}, nil
}

func (b *Backend) Name() string { return "docker" }
func (b *Backend) Close() error { return b.cli.Close() }

func (b *Backend) Create(ctx context.Context, s spec.Spec, labels map[string]string) (_ backend.Sandbox, retErr error) {
	if err := b.ensureImage(ctx, s.Image); err != nil {
		return nil, err
	}
	session := labels[backend.LabelSession]
	env := []string{"HOME=/workspace", "EPHEMERIS=1"}
	hc := hostConfig(s)
	var netCfg *network.NetworkingConfig
	if s.Network.Mode == "allowlist" {
		if session == "" {
			return nil, fmt.Errorf("network.mode allowlist needs the %s label", backend.LabelSession)
		}
		// resolve everything that can fail cheaply before creating anything
		launch, err := b.resolveProxy(ctx)
		if err != nil {
			return nil, err
		}
		if err := b.ensureImage(ctx, launch.image); err != nil {
			return nil, fmt.Errorf("egress proxy image: %w", err)
		}
		// fail closed: from here on, any error removes everything this
		// session created, so a half-built sandbox never runs or lingers
		defer func() {
			if retErr != nil {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if cerr := b.removeSession(cctx, "", session); cerr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("cleanup after failed create: %w", cerr))
				}
			}
		}()
		if err := b.startEgress(ctx, s, labels, launch); err != nil {
			return nil, fmt.Errorf("network allowlist: %w", err)
		}
		// the sandbox's one and only network has no route out; the proxy is
		// the single reachable peer
		hc.NetworkMode = container.NetworkMode(networkName(session))
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			networkName(session): {},
		}}
		env = append(env, proxyEnv()...)
	}
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	cfg := &container.Config{
		Image: s.Image,
		// keep PID 1 alive and boring; real work happens through Exec
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		WorkingDir: "/workspace",
		Env:        env,
		Labels:     labels,
		Hostname:   "ephemeris",
	}
	cfg.ExposedPorts, hc.PortBindings = publish(s.Network.Ports)
	name := "ephemeris-" + session
	resp, err := b.cli.ContainerCreate(ctx, cfg, hc, netCfg, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	return &sandbox{b: b, id: resp.ID, session: session}, nil
}

// hostConfig is the security posture
func hostConfig(s spec.Spec) *container.HostConfig {
	initProc := true
	pids := s.Resources.PIDs
	disk := fmt.Sprintf("size=%d", s.Resources.Disk)
	// deny by default: no interface except loopback. allowlist starts here
	// too, and Create swaps in the session's internal network
	net := "none"
	if s.Network.Mode == "open" {
		net = "bridge"
	}
	return &container.HostConfig{
		Init:           &initProc, // tiny init reaps zombies and forwards signals
		ReadonlyRootfs: true,      // only the tmpfs mounts below are writable
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"}, // setuid binaries cannot escalate
		NetworkMode:    container.NetworkMode(net),
		Runtime:        s.Runtime,
		Tmpfs: map[string]string{ // RAM-backed: nothing touches disk, nothing survives
			"/workspace":   "rw,nosuid,nodev,mode=0777," + disk,
			"/tmp":         "rw,nosuid,nodev,mode=1777," + disk,
			"/run/secrets": "rw,noexec,nosuid,nodev,mode=0700,size=1m",
		},
		Resources: container.Resources{
			NanoCPUs:  int64(s.Resources.CPU * 1e9),
			Memory:    int64(s.Resources.Memory),
			PidsLimit: &pids, // fork bombs stop here
		},
	}
}

// publish binds each port to 127.0.0.1 only. Binding to 0.0.0.0 (the engine
// default) would hand your sandbox to everyone on the café Wi-Fi
func publish(ports []int) (nat.PortSet, nat.PortMap) {
	if len(ports) == 0 {
		return nil, nil
	}
	set, bindings := nat.PortSet{}, nat.PortMap{}
	for _, p := range ports {
		port := nat.Port(fmt.Sprintf("%d/tcp", p))
		set[port] = struct{}{}
		bindings[port] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: fmt.Sprint(p)}}
	}
	return set, bindings
}

func (b *Backend) ensureImage(ctx context.Context, ref string) error {
	if _, err := b.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	rc, err := b.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	return jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil)
}

// List returns one Info per session, so callers (the reaper, ls) see
// sandboxes, not plumbing. a session is represented by its sandbox container;
// if that is gone (crash mid-create or mid-teardown), by its orphaned proxy;
// failing that, by its orphaned network. Remove of any of those IDs removes
// the whole session
func (b *Backend) List(ctx context.Context) ([]backend.Info, error) {
	managed := filters.NewArgs(filters.Arg("label", backend.LabelManaged+"=true"))
	cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: managed})
	if err != nil {
		return nil, err
	}
	nets, err := b.cli.NetworkList(ctx, network.ListOptions{Filters: managed})
	if err != nil {
		return nil, err
	}
	const (
		rankNetwork = iota
		rankProxy
		rankSandbox
	)
	type pick struct {
		info backend.Info
		rank int
	}
	var (
		order []string
		best  = map[string]pick{}
		out   []backend.Info
	)
	consider := func(info backend.Info, rank int) {
		sess := info.Labels[backend.LabelSession]
		if sess == "" { // not ours to group; list it as-is
			out = append(out, info)
			return
		}
		cur, seen := best[sess]
		if !seen {
			order = append(order, sess)
		}
		if !seen || rank > cur.rank {
			best[sess] = pick{info, rank}
		}
	}
	for _, c := range cs {
		rank := rankSandbox
		if c.Labels[LabelRole] == RoleProxy {
			rank = rankProxy
		}
		consider(backend.Info{ID: c.ID, Labels: c.Labels, Created: time.Unix(c.Created, 0)}, rank)
	}
	for _, n := range nets {
		consider(backend.Info{ID: n.ID, Labels: n.Labels, Created: n.Created}, rankNetwork)
	}
	for _, sess := range order {
		out = append(out, best[sess].info)
	}
	return out, nil
}

// Remove takes a container or network ID from List and removes the whole
// session it belongs to: sandbox, egress proxy and internal network.
// removing something already gone is not an error
func (b *Backend) Remove(ctx context.Context, id string) error {
	c, err := b.cli.ContainerInspect(ctx, id)
	if err == nil {
		sess := ""
		if c.Config != nil {
			sess = c.Config.Labels[backend.LabelSession]
		}
		return b.removeSession(ctx, c.ID, sess)
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspect %s: %w", short(id), err)
	}
	n, err := b.cli.NetworkInspect(ctx, id, network.InspectOptions{})
	if client.IsErrNotFound(err) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("inspect network %s: %w", short(id), err)
	}
	if n.Labels[backend.LabelManaged] != "true" || n.Labels[backend.LabelSession] == "" {
		return fmt.Errorf("refusing to remove network %s: not an Ephemeris session network", n.Name)
	}
	return b.removeSession(ctx, "", n.Labels[backend.LabelSession])
}

// EngineInfo feeds `ephemeris doctor`
func (b *Backend) EngineInfo(ctx context.Context) (version string, rootless bool, runtimes []string, err error) {
	info, err := b.cli.Info(ctx)
	if err != nil {
		return "", false, nil, err
	}
	for _, o := range info.SecurityOptions {
		if strings.Contains(o, "rootless") {
			rootless = true
		}
	}
	for name := range info.Runtimes {
		runtimes = append(runtimes, name)
	}
	return info.ServerVersion, rootless, runtimes, nil
}

type sandbox struct {
	b       *Backend
	id      string
	session string
}

func (s *sandbox) ID() string { return s.id }
func (s *sandbox) Start(ctx context.Context) error {
	return s.b.cli.ContainerStart(ctx, s.id, container.StartOptions{})
}

// Destroy uses the session it was created with, so a second call still
// cleans up the proxy and network even after the sandbox container is gone
func (s *sandbox) Destroy(ctx context.Context) error { return s.b.removeSession(ctx, s.id, s.session) }

func (s *sandbox) Exec(ctx context.Context, req backend.ExecRequest) (int, error) {
	opts := container.ExecOptions{
		Cmd: req.Cmd, Env: req.Env, WorkingDir: req.WorkDir, Tty: req.TTY,
		AttachStdin: req.Stdin != nil, AttachStdout: true, AttachStderr: true,
	}
	if req.TTY && req.InitialSize.Rows > 0 {
		opts.ConsoleSize = &[2]uint{uint(req.InitialSize.Rows), uint(req.InitialSize.Cols)}
	}
	created, err := s.b.cli.ContainerExecCreate(ctx, s.id, opts)
	if err != nil {
		return -1, fmt.Errorf("exec create: %w", err)
	}
	// attach upgrades the HTTP connection into a raw bidirectional stream
	hj, err := s.b.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: req.TTY, ConsoleSize: opts.ConsoleSize})
	if err != nil {
		return -1, fmt.Errorf("exec attach: %w", err)
	}
	defer hj.Close()
	// a blocked Read ignores ctx. closing the conn on cancel is what unblocks it
	stop := context.AfterFunc(ctx, hj.Close)
	defer stop()

	if opts.ConsoleSize != nil {
		// some engines (Podman) ignore ConsoleSize on create; set it explicitly too
		_ = s.b.cli.ContainerExecResize(ctx, created.ID, container.ResizeOptions{Height: opts.ConsoleSize[0], Width: opts.ConsoleSize[1]})
	}
	if req.Resize != nil {
		go func() {
			for sz := range req.Resize {
				_ = s.b.cli.ContainerExecResize(ctx, created.ID, container.ResizeOptions{Height: uint(sz.Rows), Width: uint(sz.Cols)})
			}
		}()
	}
	if req.Stdin != nil {
		go func() {
			_, _ = io.Copy(hj.Conn, req.Stdin)
			_ = hj.CloseWrite() // half-close: the process sees EOF on stdin
		}()
	}

	if req.TTY {
		_, err = io.Copy(backend.OrDiscard(req.Stdout), hj.Reader)
	} else {
		// without a TTY, stdout and stderr share one stream with 8-byte frame headers
		_, err = stdcopy.StdCopy(backend.OrDiscard(req.Stdout), backend.OrDiscard(req.Stderr), hj.Reader)
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if err != nil {
		return -1, fmt.Errorf("exec stream: %w", err)
	}
	// the stream can close a moment before the engine records the exit code
	for range 20 {
		insp, err := s.b.cli.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return -1, err
		}
		if !insp.Running {
			return insp.ExitCode, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return -1, fmt.Errorf("exec %s did not report an exit code", short(created.ID))
}

func short(id string) string { return id[:min(12, len(id))] }
