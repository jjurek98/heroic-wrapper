package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Re-exec only for our private helper mode, before normal CLI/TUI dispatch.
// Keeping the entry point here leaves list/show/where and their output alone.
// A request does not grant privileges: sudo must already have authorized this
// executable and the helper refuses to operate without effective UID 0.
func init() {
	if len(os.Args) != 2 || os.Args[1] != lanHelperArgument {
		return
	}
	if err := runLANHelper(); err != nil {
		fmt.Fprintln(os.Stderr, "heroic-wrapper LAN:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type lanWire struct {
	mu     sync.Mutex
	out    *json.Encoder
	cancel context.CancelFunc
}

func (w *lanWire) send(event lanEvent) {
	w.mu.Lock()
	err := w.out.Encode(event)
	w.mu.Unlock()
	if err != nil {
		w.cancel()
	}
}

func (w *lanWire) Write(p []byte) (int, error) {
	// Bound individual protocol frames, even for newline-free process output.
	for rest := p; len(rest) > 0; {
		n := min(len(rest), 4096)
		w.send(lanEvent{Kind: "log", Data: rest[:n]})
		rest = rest[n:]
	}
	return len(p), nil
}

// Interpret only lanparty's stdout startup banner, not arbitrary stderr logs.
// In upstream both server and client print Interface: only after connection
// initialization succeeds. The actual address is subsequently read from ip.
type lanBanner struct {
	wire          *lanWire
	line          []byte
	discard       bool
	interfaceSeen bool
	tokenSeen     bool
	ready         chan string
}

func (b *lanBanner) Write(p []byte) (int, error) {
	_, _ = b.wire.Write(p)
	for _, c := range p {
		if c == '\n' {
			if !b.discard {
				line := strings.TrimSpace(string(b.line))
				if value, ok := strings.CutPrefix(line, "Interface:"); ok && !b.interfaceSeen {
					value = strings.TrimSpace(value)
					if validLANInterface(value) {
						b.interfaceSeen = true
						b.ready <- value
					}
				}
				if value, ok := strings.CutPrefix(line, "Token:"); ok && !b.tokenSeen {
					value = strings.TrimSpace(value)
					if validateLANToken(value) == nil {
						b.tokenSeen = true
						b.wire.send(lanEvent{Kind: "token", Token: value})
					}
				}
			}
			b.line, b.discard = b.line[:0], false
		} else if len(b.line) < lanTokenLimit+64 && !b.discard {
			b.line = append(b.line, c)
		} else {
			b.discard = true
		}
	}
	return len(p), nil
}

func validLANInterface(name string) bool {
	if !strings.HasPrefix(name, "lp") || len(name) < 3 || len(name) > 15 {
		return false
	}
	for _, c := range name[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type lanAddress struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
}

type lanLink struct {
	Name      string       `json:"ifname"`
	Index     int          `json:"ifindex"`
	MTU       int          `json:"mtu"`
	Addresses []lanAddress `json:"addr_info"`
}

type lanNetwork struct {
	ip          string
	wire        *lanWire
	tap         lanLink
	cidr        string
	gateway     string
	inner       string
	namespace   os.FileInfo
	ownsNS      bool
	ownsVeth    bool
	ownsBridge  bool
	vethIndex   int
	bridgeIndex int
}

// Every individual ip operation is bounded, including cleanup after cancel.
func (n *lanNetwork) command(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.ip, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ip %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (n *lanNetwork) do(ctx context.Context, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Do not interrupt a mutation halfway through when the UI closes: ip
	// could have created a resource but die before returning success, losing
	// our ownership record. Finish this bounded command, record its result,
	// and observe cancellation before the next step.
	_, err := n.command(context.WithoutCancel(ctx), args...)
	return err
}

func (n *lanNetwork) links(ctx context.Context, name string) ([]lanLink, error) {
	args := []string{"-j", "address", "show"}
	if name != "" {
		args = append(args, "dev", name)
	}
	data, err := n.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var links []lanLink
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, fmt.Errorf("decode ip address JSON: %w", err)
	}
	return links, nil
}

// Record the kernel identity immediately after a successful create. Cleanup
// checks it before deleting a name, rather than deleting another administrator's
// replacement if an interface was removed and recreated during the session.
func (n *lanNetwork) linkIndex(name string) (int, error) {
	links, err := n.links(context.Background(), name)
	if err != nil {
		return 0, err
	}
	if len(links) != 1 || links[0].Index <= 0 {
		return 0, fmt.Errorf("cannot record identity of %s", name)
	}
	return links[0].Index, nil
}

func lanNamespacePath() string { return "/var/run/netns/" + lanNamespace }

func (n *lanNetwork) preflight(ctx context.Context) (map[string]int, error) {
	if _, err := os.Stat(lanNamespacePath()); err == nil {
		return nil, fmt.Errorf("network namespace %q already exists; no existing network resources were changed", lanNamespace)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	links, err := n.links(ctx, "")
	if err != nil {
		return nil, err
	}
	before := make(map[string]int, len(links))
	for _, link := range links {
		before[link.Name] = link.Index
	}
	n.inner = "eth0"
	if _, exists := before[n.inner]; exists {
		n.inner = "eth0-games"
	}
	for _, name := range []string{lanBridge, lanVeth, n.inner} {
		if _, exists := before[name]; exists {
			return nil, fmt.Errorf("interface %q already exists; refusing to replace it", name)
		}
	}
	return before, nil
}

func (n *lanNetwork) configure(ctx context.Context, name string, before map[string]int) error {
	if !validLANInterface(name) {
		return errors.New("lanparty reported an invalid TAP interface name")
	}
	if _, existed := before[name]; existed {
		return fmt.Errorf("lanparty reported pre-existing interface %q; refusing to use it", name)
	}
	links, err := n.links(ctx, name)
	if err != nil {
		return err
	}
	if len(links) != 1 || links[0].Index <= 0 || links[0].MTU <= 0 {
		return errors.New("could not identify the new lanparty interface")
	}
	n.tap = links[0]
	var prefix netip.Prefix
	for _, addr := range n.tap.Addresses {
		if addr.Family != "inet" {
			continue
		}
		p, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", addr.Local, addr.PrefixLen))
		if err != nil || !p.Addr().Is4() || prefix.IsValid() {
			return errors.New("expected exactly one IPv4 address on the new lanparty interface")
		}
		prefix = p
	}
	if !prefix.IsValid() {
		return errors.New("lanparty interface has no IPv4 address")
	}
	gateway := prefix.Addr().As4()
	gateway[3] = 254 // Preserve the gateway convention in create-games-netns.sh.
	gw := netip.AddrFrom4(gateway)
	if !prefix.Contains(gw) || gw == prefix.Addr() {
		return errors.New("assigned IPv4 address conflicts with the script's .254 gateway convention")
	}
	n.cidr, n.gateway = prefix.String(), gw.String()
	fmt.Fprintf(n.wire, "\n[heroic-wrapper] Configuring %s -> %s -> %s (%s), gateway %s\n", name, lanBridge, lanNamespace, n.cidr, n.gateway)

	// Same topology and address transfer as the supplied script. Ownership
	// flags are set only after successful creates, for partial rollback.
	if err := n.do(ctx, "link", "add", lanVeth, "type", "veth", "peer", "name", n.inner); err != nil {
		return err
	}
	n.ownsVeth = true
	n.vethIndex, err = n.linkIndex(lanVeth)
	if err != nil {
		return err
	}
	mtu := strconv.Itoa(n.tap.MTU)
	for _, args := range [][]string{
		{"link", "set", "dev", lanVeth, "mtu", mtu},
		{"link", "set", "dev", n.inner, "mtu", mtu},
		{"link", "set", "dev", lanVeth, "up"},
		{"address", "del", n.cidr, "dev", name},
	} {
		if err := n.do(ctx, args...); err != nil {
			return err
		}
	}
	if err := n.do(ctx, "link", "add", "dev", lanBridge, "type", "bridge"); err != nil {
		return err
	}
	n.ownsBridge = true
	n.bridgeIndex, err = n.linkIndex(lanBridge)
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"link", "set", "dev", lanBridge, "mtu", mtu},
		{"link", "set", "dev", lanBridge, "up"},
		{"link", "set", "dev", lanVeth, "master", lanBridge},
		{"link", "set", "dev", name, "master", lanBridge},
	} {
		if err := n.do(ctx, args...); err != nil {
			return err
		}
	}
	if err := n.do(ctx, "netns", "add", lanNamespace); err != nil {
		return err
	}
	n.ownsNS = true
	n.namespace, err = os.Stat(lanNamespacePath())
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"link", "set", "dev", n.inner, "netns", lanNamespace},
		{"-n", lanNamespace, "link", "set", "dev", "lo", "up"},
		{"-n", lanNamespace, "link", "set", "dev", n.inner, "up"},
		{"-n", lanNamespace, "address", "add", n.cidr, "dev", n.inner},
		{"-n", lanNamespace, "route", "add", "default", "via", n.gateway, "dev", n.inner},
	} {
		if err := n.do(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (n *lanNetwork) namespacePIDs(ctx context.Context) ([]int, error) {
	out, err := n.command(ctx, "netns", "pids", lanNamespace)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, value := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(value)
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		// Never signal a PID that has already left our namespace (or was
		// reused since ip enumerated it).
		info, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
		if err == nil && n.namespace != nil && os.SameFile(info, n.namespace) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (n *lanNetwork) stopNamespaceProcesses(ctx context.Context) error {
	pids, err := n.namespacePIDs(ctx)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(pids) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		pids, err = n.namespacePIDs(ctx)
		if err != nil {
			return err
		}
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func (n *lanNetwork) removeLink(ctx context.Context, name string, index int, owned *bool) error {
	if !*owned {
		return nil
	}
	links, err := n.links(ctx, "")
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Name != name {
			continue
		}
		if index == 0 || link.Index != index {
			return fmt.Errorf("cannot verify ownership of %s; refusing to delete it", name)
		}
		if err := n.do(ctx, "link", "delete", "dev", name); err != nil {
			return err
		}
		break
	}
	*owned = false
	return nil
}

func (n *lanNetwork) cleanup() error {
	// Do not reuse the canceled startup context. The still-privileged helper
	// must finish rollback even after EOF, a startup timeout, or SIGTERM.
	ctx := context.Background()
	var errs []error
	if n.ownsNS {
		info, err := os.Stat(lanNamespacePath())
		switch {
		case errors.Is(err, os.ErrNotExist):
			n.ownsNS = false
		case err != nil:
			errs = append(errs, err)
		case n.namespace != nil && !os.SameFile(info, n.namespace):
			errs = append(errs, errors.New("games namespace was replaced externally; refusing to delete the replacement"))
		default:
			// A namespace mount alone is not its lifetime: games must exit
			// before deleting it, or their veth endpoint can stay alive.
			errs = append(errs, n.stopNamespaceProcesses(ctx))
			if err := n.do(ctx, "netns", "delete", lanNamespace); err != nil {
				errs = append(errs, err)
			} else {
				n.ownsNS = false
			}
		}
	}
	// Normally namespace deletion removes the veth pair. Also remove the
	// owned host end if startup failed before moving its peer, or an external
	// namespace FD still pins the namespace after its name was deleted.
	errs = append(errs, n.removeLink(ctx, lanVeth, n.vethIndex, &n.ownsVeth))
	errs = append(errs, n.removeLink(ctx, lanBridge, n.bridgeIndex, &n.ownsBridge))
	return errors.Join(errs...)
}

func stopLANProcess(cmd *exec.Cmd, done <-chan error) error {
	select {
	case <-done:
		return nil
	default:
	}
	// Upstream handles SIGTERM/SIGINT and closes TAP and tailcat resources.
	// SIGKILL cannot run that cleanup, so it is only a bounded fallback.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return errors.New("lanparty did not stop after SIGTERM; SIGKILL was required")
	}
}

func runLANHelper() (result error) {
	if os.Geteuid() != 0 {
		return errors.New("the internal LAN helper requires sudo privileges")
	}
	// Let a broken UI pipe cancel our context instead of exiting on SIGPIPE
	// before the deferred network cleanup has had a chance to run.
	signal.Ignore(syscall.SIGPIPE)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	wire := &lanWire{out: json.NewEncoder(os.Stdout), cancel: cancel}
	defer func() {
		if result != nil {
			wire.send(lanEvent{Kind: "error", Text: result.Error()})
		}
	}()
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), 32*1024)
	if !input.Scan() {
		return errors.New("missing LAN helper request")
	}
	var request lanRequest
	if err := json.Unmarshal(input.Bytes(), &request); err != nil {
		return errors.New("invalid LAN helper request")
	}
	for _, path := range []string{request.Binary, request.IP} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
			return errors.New("LAN helper requires absolute executable paths")
		}
	}
	if !request.Server {
		if err := validateLANToken(request.Token); err != nil {
			return err
		}
	}
	go func() {
		// Only a single initial request is accepted. Further input, EOF or
		// a broken pipe means stop, never a new privileged instruction.
		input.Scan()
		cancel()
	}()
	network := &lanNetwork{ip: request.IP, wire: wire}
	before, err := network.preflight(ctx)
	if err != nil {
		return err
	}
	ready := make(chan string, 1)
	args := []string{"--copy-token=false"} // No desktop clipboard access as root.
	if !request.Server {
		args = append(args, "--", request.Token)
	}
	cmd := exec.Command(request.Binary, args...)
	cmd.Stdout = &lanBanner{wire: wire, ready: ready}
	cmd.Stderr = wire
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start installed lanparty: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() {
		fmt.Fprintln(wire, "\n[heroic-wrapper] Removing the LAN namespace, veth and bridge...")
		cleanupErr := network.cleanup()
		if cleanupErr != nil {
			fmt.Fprintln(wire, "[heroic-wrapper] Retrying incomplete network cleanup:", cleanupErr)
			cleanupErr = network.cleanup()
		}
		result = errors.Join(result, cleanupErr)
		result = errors.Join(result, stopLANProcess(cmd, done))
		if result == nil {
			fmt.Fprintln(wire, "[heroic-wrapper] LAN shutdown completed.")
		} else {
			fmt.Fprintln(wire, "[heroic-wrapper] LAN stopped; errors are reported below.")
		}
	}()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return fmt.Errorf("lanparty exited before its interface was ready: %v", err)
	case <-timer.C:
		return errors.New("lanparty did not report a ready interface within 90 seconds")
	case name := <-ready:
		if err := network.configure(ctx, name, before); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	select {
	case err := <-done:
		return fmt.Errorf("lanparty exited during network configuration: %v", err)
	case <-ctx.Done():
		return nil
	default:
	}
	wire.send(lanEvent{Kind: "ready", Interface: network.tap.Name, CIDR: network.cidr})
	fmt.Fprintln(wire, "[heroic-wrapper] LAN ready. Select the games namespace when launching a game.")
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		text := "lanparty has disconnected. Running games have not been stopped. Disconnect the LAN or exit Heroic Wrapper to remove its network resources."
		if err != nil {
			text += " Process error: " + err.Error()
		}
		fmt.Fprintln(wire, "[heroic-wrapper]", text)
		wire.send(lanEvent{Kind: "offline", Text: text})
		// Losing a remote server must not unexpectedly destroy unsaved game
		// progress. Retain ownership until the user confirms disconnect/quit,
		// or the UI disappears (EOF). No sudo renewal is needed at that point.
		<-ctx.Done()
		return nil
	}
}
