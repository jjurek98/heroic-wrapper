package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// An empty name means the namespace inherited by heroic-wrapper, not
// necessarily the host's default namespace. Discovery never needs sudo.
func networkNamespaces() ([]string, error) {
	names := []string{""}
	entries, err := os.ReadDir("/var/run/netns")
	if errors.Is(err, os.ErrNotExist) {
		entries, err = os.ReadDir("/run/netns")
	}
	if errors.Is(err, os.ErrNotExist) {
		return names, nil
	}
	if err != nil {
		return names, fmt.Errorf("list network namespaces: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && validNamespace(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func validNamespace(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.HasPrefix(name, "-") && !strings.ContainsAny(name, "/\x00\n\r")
}

type launchPlan struct {
	program string
	args    []string
	env     []string
	sudo    string
}

func launchBinary(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("launch requires %s in PATH: %w", name, err)
	}
	return filepath.Abs(path)
}

// The generated shell program is always interpreted AFTER setpriv drops
// privileges. Namespace names and user IDs are separate argv entries, never
// interpolated into shell code. No temporary scripts or eval are needed.
func prepareLaunch(command, namespace string) (*launchPlan, error) {
	bash, err := launchBinary("bash")
	if err != nil {
		return nil, err
	}
	plan := &launchPlan{program: bash, args: []string{"-c", command}, env: os.Environ()}
	if namespace == "" {
		return plan, nil
	}
	if !validNamespace(namespace) {
		return nil, fmt.Errorf("invalid network namespace %q", namespace)
	}
	if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		return nil, errors.New("run heroic-wrapper without setuid/setgid privileges")
	}
	ip, err := launchBinary("ip")
	if err != nil {
		return nil, err
	}
	setpriv, err := launchBinary("setpriv")
	if err != nil {
		return nil, err
	}
	env, err := launchBinary("env")
	if err != nil {
		return nil, err
	}
	plan.program = ip
	plan.args = []string{
		"netns", "exec", namespace, setpriv,
		"--reuid=" + strconv.Itoa(os.Getuid()),
		"--regid=" + strconv.Itoa(os.Getgid()), "--init-groups", "--", env,
	}
	// sudo may rewrite HOME/USER/PATH or strip loader/session variables even
	// with -E. Restore these only in the unprivileged child, never in ip or
	// setpriv. Other variables follow the user's sudo environment policy.
	for _, key := range []string{
		"HOME", "USER", "LOGNAME", "PATH", "XDG_RUNTIME_DIR",
		"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS",
		"PULSE_SERVER", "PIPEWIRE_REMOTE", "LD_LIBRARY_PATH", "LD_PRELOAD",
		"ORIG_LD_LIBRARY_PATH",
	} {
		if value, ok := os.LookupEnv(key); ok {
			plan.args = append(plan.args, key+"="+value)
		}
	}
	plan.args = append(plan.args, bash, "-c", command)
	if os.Geteuid() != 0 {
		plan.sudo, err = launchBinary("sudo")
		if err != nil {
			return nil, err
		}
		// Authenticate separately through the TUI password dialog. -n prevents
		// a late password prompt from competing with the UI or entering logs.
		plan.args = append([]string{"-n", "-E", "--", ip}, plan.args...)
		plan.program = plan.sudo
	}
	return plan, nil
}

type gameSession struct {
	cancel  context.CancelFunc
	done    chan struct{}
	settled chan struct{}
	err     error // read only after done is closed
}

func startGame(parent context.Context, plan *launchPlan, output *gameLog) (*gameSession, error) {
	ctx, cancel := context.WithCancel(parent)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	cmd := exec.Command(plan.program, plan.args...)
	cmd.Env = plan.env
	cmd.Stdout, cmd.Stderr = output, output
	// Games must not consume the TUI's input. A nil Stdin gives /dev/null.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	session := &gameSession{cancel: cancel, done: make(chan struct{}), settled: make(chan struct{})}
	go func() {
		session.err = cmd.Wait()
		close(session.done)
	}()
	go func() {
		defer close(session.settled)
		defer cancel()
		select {
		case <-session.done:
			if ctx.Err() == nil {
				return
			}
		case <-ctx.Done():
		}
		// Target the launched process group, not heroic-wrapper's own group.
		// sudo forwards termination to the command it supervises.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-session.done:
		case <-timer.C:
		}
		// Also clean up group members left behind by an exiting shell.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()
	return session, nil
}

// Serialize starting with shutdown so quitting during an asynchronous Start
// cannot leave an untracked game behind after the UI has already returned.
type gameLauncher struct {
	mu      sync.Mutex
	closed  bool
	session *gameSession
}

func (launcher *gameLauncher) start(ctx context.Context, plan *launchPlan, output *gameLog) (*gameSession, error) {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if launcher.closed {
		return nil, context.Canceled
	}
	session, err := startGame(ctx, plan, output)
	if err == nil {
		launcher.session = session
	}
	return session, err
}

func (launcher *gameLauncher) close() {
	launcher.mu.Lock()
	launcher.closed = true
	session := launcher.session
	launcher.mu.Unlock()
	if session != nil {
		session.cancel()
		<-session.done
		<-session.settled
	}
}
