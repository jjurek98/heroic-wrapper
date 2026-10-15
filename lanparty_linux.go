package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	lanHelperArgument = "--internal-lanparty-helper"
	lanNamespace      = "games"
	lanBridge         = "br-games"
	lanVeth           = "veth-games"
	lanTokenLimit     = 4096
)

var errLANNotInstalled = errors.New("lanparty is not installed or is not executable in PATH; install rafalb8/lanparty yourself, then try again. Nothing was downloaded")

// Only this fixed request crosses the privilege boundary. The helper does not
// accept shell snippets, arbitrary network commands, or cleanup targets.
type lanRequest struct {
	Binary string `json:"binary"`
	IP     string `json:"ip"`
	Server bool   `json:"server"`
	Token  string `json:"token,omitempty"`
}

type lanEvent struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Interface string `json:"interface,omitempty"`
	CIDR      string `json:"cidr,omitempty"`
	Token     string `json:"token,omitempty"`
}

type lanPlan struct {
	request lanRequest
	helper  string
	sudo    string
}

func validateLANToken(token string) error {
	if len(token) == 0 || len(token) > lanTokenLimit {
		return errors.New("paste one complete server token (at most 4096 bytes)")
	}
	for _, r := range token {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("the server token must not contain whitespace or control characters")
		}
	}
	return nil
}

func prepareLAN(server bool, token string) (*lanPlan, error) {
	binary, err := exec.LookPath("lanparty")
	if err != nil {
		return nil, errLANNotInstalled
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if !server {
		token = strings.TrimSpace(token)
		if err := validateLANToken(token); err != nil {
			return nil, err
		}
	} else {
		token = ""
	}
	ip, err := launchBinary("ip")
	if err != nil {
		return nil, err
	}
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	plan := &lanPlan{helper: helper, request: lanRequest{Binary: binary, IP: ip, Server: server, Token: token}}
	if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		return nil, errors.New("run heroic-wrapper without setuid/setgid privileges")
	}
	if os.Geteuid() != 0 {
		plan.sudo, err = launchBinary("sudo")
	}
	return plan, err
}

type lanSession struct {
	cancel func()
	done   chan struct{}
	events chan lanEvent // Lifecycle events only; logs go straight to the ring.
	err    error         // Read only after done is closed.
}

// EOF on the control pipe requests cleanup. The root helper owns the network
// resources for their whole lifetime; cleanup never depends on a still-valid
// sudo timestamp, and also runs if the UI crashes or its terminal disappears.
func startLAN(parent context.Context, plan *lanPlan, output *gameLog) (*lanSession, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	program, args := plan.helper, []string{lanHelperArgument}
	if plan.sudo != "" {
		program, args = plan.sudo, []string{"-n", "--", plan.helper, lanHelperArgument}
	}
	cmd := exec.Command(program, args...)
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	control, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	wire, err := cmd.StdoutPipe()
	if err != nil {
		control.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		control.Close()
		wire.Close()
		return nil, err
	}
	session := &lanSession{done: make(chan struct{}), events: make(chan lanEvent, 8)}
	var once sync.Once
	session.cancel = func() { once.Do(func() { _ = control.Close() }) }
	go func() {
		defer close(session.done)
		defer close(session.events)
		defer session.cancel()
		decoder := json.NewDecoder(wire)
		var reported error
		for {
			var event lanEvent
			if err := decoder.Decode(&event); err != nil {
				if !errors.Is(err, io.EOF) {
					reported = errors.Join(reported, fmt.Errorf("LAN helper protocol: %w", err))
					session.cancel()
					_, _ = io.Copy(io.Discard, wire)
				}
				break
			}
			switch event.Kind {
			case "log":
				_, _ = output.Write(event.Data)
			case "error":
				reported = errors.Join(reported, errors.New(event.Text))
				fmt.Fprintln(output, "\n[heroic-wrapper]", event.Text)
			case "ready", "token", "offline":
				// The helper emits at most one of each per connection. Never
				// block the log reader if shutdown races with a final event.
				select {
				case session.events <- event:
				default:
				}
			}
		}
		// Finish consuming stdout before Wait closes the read pipe.
		session.err = errors.Join(reported, cmd.Wait())
	}()
	go func() {
		select {
		case <-parent.Done():
			session.cancel()
		case <-session.done:
		}
	}()
	if err := json.NewEncoder(control).Encode(plan.request); err != nil {
		session.cancel()
		<-session.done
		return nil, fmt.Errorf("start LAN helper: %w", err)
	}
	return session, nil
}

// Like gameLauncher, this closes the Start/quit race even if Bubble Tea stops
// consuming result messages while the operating system is starting sudo.
type lanLauncher struct {
	mu      sync.Mutex
	closed  bool
	session *lanSession
}

func (l *lanLauncher) start(ctx context.Context, plan *lanPlan, output *gameLog) (*lanSession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, context.Canceled
	}
	session, err := startLAN(ctx, plan, output)
	if err == nil {
		l.session = session
	}
	return session, err
}

func (l *lanLauncher) close() {
	l.mu.Lock()
	l.closed = true
	session := l.session
	l.mu.Unlock()
	if session != nil {
		session.cancel()
		<-session.done
	}
}
