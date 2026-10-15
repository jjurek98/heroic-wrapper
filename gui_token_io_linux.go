package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// copyLANToken uses the desktop clipboard as the current user, never sudo.
// The token is stdin data, not a shell argument, environment entry or file.
func copyLANToken(ctx context.Context, token string, terminal *os.File) (string, error) {
	var commands [][]string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		commands = append(commands, []string{"wl-copy", "--type", "text/plain"})
	}
	if os.Getenv("DISPLAY") != "" {
		commands = append(commands, []string{"xclip", "-selection", "clipboard", "-in"},
			[]string{"xsel", "--clipboard", "--input"})
	}
	for _, args := range commands {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		binary, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		copyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		cmd := exec.CommandContext(copyCtx, binary, args[1:]...)
		cmd.Stdin = strings.NewReader(token)
		// Leave stdout/stderr on /dev/null. Clipboard owners can fork and
		// keep those descriptors open; CombinedOutput could hang until paste.
		// Do not use --paste-once: the token may need to be shared repeatedly.
		err = cmd.Run()
		cancel()
		if err == nil {
			return "Token copied to clipboard (" + args[0] + ").", nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// OSC 52 also works without desktop utilities (for example over SSH),
	// subject to the terminal's clipboard permissions. It has no success
	// acknowledgement: report a request, not a confirmed clipboard write.
	term := os.Getenv("TERM")
	if terminal == nil || term == "" || term == "dumb" || term == "linux" {
		return "", errors.New("no working clipboard helper or clipboard-capable terminal")
	}
	if _, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS); err != nil {
		return "", fmt.Errorf("clipboard output is not a terminal: %w", err)
	}
	sequence := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(token)) + "\a"
	// One write on the same *os.File as the renderer keeps the escape string
	// together without pausing/releasing the fullscreen UI.
	if _, err := io.WriteString(terminal, sequence); err != nil {
		return "", fmt.Errorf("send clipboard request: %w", err)
	}
	return "Clipboard request sent to terminal (OSC 52). Paste to verify; press r if blocked.", nil
}

// rawTokenView implements tea.ExecCommand without a subprocess. Bubble Tea
// owns releasing/restoring its terminal, renderer, input reader and screen.
type rawTokenView struct {
	ctx    context.Context
	token  string
	input  io.Reader
	output io.Writer
}

func (v *rawTokenView) SetStdin(r io.Reader)  { v.input = r }
func (v *rawTokenView) SetStdout(w io.Writer) { v.output = w }
func (v *rawTokenView) SetStderr(io.Writer)   {}

func (v *rawTokenView) Run() (retErr error) {
	if err := v.ctx.Err(); err != nil {
		return err
	}
	input, ok := v.input.(*os.File)
	if !ok || v.output == nil {
		return errors.New("raw token view requires a terminal")
	}
	fd := int(input.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("read terminal settings: %w", err)
	}
	settings := *original
	// Keep normal output/newline processing and terminal text selection.
	// Read a key immediately, without echo; Ctrl+C/Z are return keys here,
	// not signals to the running game or the LAN helper.
	settings.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG | unix.IEXTEN
	settings.Iflag &^= unix.IXON | unix.ICRNL | unix.INLCR
	settings.Cc[unix.VMIN], settings.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &settings); err != nil {
		return fmt.Errorf("set terminal input mode: %w", err)
	}
	defer func() {
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, original); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore terminal settings: %w", err))
		}
	}()
	if _, err := fmt.Fprintf(v.output, "\nLAN server token\n\n%s\n\nSelect the token with the mouse and copy using your terminal.\nPress any key to return to Heroic Wrapper.\nThe token remains in terminal scrollback; share it only with trusted players.\n", v.token); err != nil {
		return err
	}
	return waitForTokenKey(v.ctx, fd)
}

func waitForTokenKey(ctx context.Context, fd int) error {
	var buffer [4096]byte
	var received time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := 100
		if !received.IsZero() {
			// Consume a whole key sequence/paste burst rather than leaking an
			// arrow-key suffix (or pasted q) to the resumed fullscreen UI.
			if time.Since(received) >= 250*time.Millisecond {
				return nil
			}
			timeout = 25
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			if !received.IsZero() {
				return nil
			}
			continue
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			return io.EOF
		}
		n, err = unix.Read(fd, buffer[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		if received.IsZero() {
			received = time.Now()
		}
	}
}
