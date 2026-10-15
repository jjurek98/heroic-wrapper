package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type dialogKind uint8

const (
	dialogNone dialogKind = iota
	dialogPassword
	dialogStop
	dialogQuit
	dialogLANMode
	dialogLANJoin
	dialogLANInfo
	dialogLANStop
	maxPasswordBytes = 4096
)

type guiDialog struct {
	kind     dialogKind
	password []byte
	problem  string
	title    string
	text     string
	confirm  bool // Cancel is selected by default.
}

type guiSudoPrompt struct {
	binary, target, reason string
	proceed                func() tea.Cmd
	abort                  func()
}

type sudoAttempt struct {
	cancel context.CancelFunc
	probe  bool
}

func (m *guiModel) dismissDialog() {
	if m.auth != nil {
		m.auth.cancel()
		m.auth = nil
	}
	clear(m.dialog.password)
	m.dialog = guiDialog{}
	m.sudoPrompt = nil
}

func (m *guiModel) continuePrepared() tea.Cmd {
	return m.requestSudo(m.plan.sudo, "Network namespace: "+namespaceLabel(m.network),
		fmt.Sprintf("Administrator permission is needed to enter the selected network namespace. The game still runs as your user (UID %d, GID %d).", os.Getuid(), os.Getgid()),
		m.startPrepared, func() {
			m.busy, m.plan = false, nil
			m.status = "Launch canceled."
		})
}

func (m *guiModel) requestSudo(binary, target, reason string, proceed func() tea.Cmd, abort func()) tea.Cmd {
	if binary == "" {
		return proceed()
	}
	m.dialog = guiDialog{kind: dialogPassword}
	m.sudoPrompt = &guiSudoPrompt{binary: binary, target: target, reason: reason, proceed: proceed, abort: abort}
	m.status = "Checking sudo authentication..."
	return m.authenticate(nil, true)
}

func (m *guiModel) authenticate(password []byte, probe bool) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	attempt := &sudoAttempt{cancel: cancel, probe: probe}
	m.auth = attempt
	m.dialog.problem = ""
	sudo := m.sudoPrompt.binary
	return func() tea.Msg {
		defer cancel()
		return authenticatedMsg{attempt, validateSudo(ctx, sudo, password, probe)}
	}
}

func (m *guiModel) updateDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dialog.kind == dialogLANMode || m.dialog.kind == dialogLANJoin || m.dialog.kind == dialogLANInfo {
		return m.updateLANDialog(msg)
	}
	if m.dialog.kind == dialogPassword {
		// Match key types, not String(): an IME can deliver a whole word in
		// one KeyRunes event, and secrets should not become immutable strings.
		if msg.Alt {
			return m, nil
		}
		if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
			abort := m.sudoPrompt.abort
			m.dismissDialog()
			abort()
			return m, nil
		}
		if m.auth != nil {
			return m, nil // Only cancellation is allowed during validation.
		}
		switch msg.Type {
		case tea.KeyEnter, tea.KeyCtrlJ:
			if len(m.dialog.password) == 0 {
				m.dialog.problem = "Enter the password required by sudo, or press esc to cancel."
				return m, nil
			}
			// Transfer ownership, rather than retaining a second copy in the
			// model or putting a password inside a Bubble Tea result message.
			password := m.dialog.password
			m.dialog.password = nil
			return m, m.authenticate(password, false)
		case tea.KeyBackspace, tea.KeyCtrlH:
			_, size := utf8.DecodeLastRune(m.dialog.password)
			end := len(m.dialog.password) - size
			clear(m.dialog.password[end:])
			m.dialog.password = m.dialog.password[:end]
			m.dialog.problem = ""
		case tea.KeyCtrlU:
			clear(m.dialog.password)
			m.dialog.password = m.dialog.password[:0]
			m.dialog.problem = ""
		case tea.KeyRunes, tea.KeySpace:
			runes := msg.Runes
			if msg.Type == tea.KeySpace {
				runes = []rune{' '}
			}
			size := 0
			for _, r := range runes {
				if !utf8.ValidRune(r) || unicode.IsControl(r) {
					m.dialog.problem = "Use a single-line password without control characters."
					return m, nil
				}
				size += utf8.RuneLen(r)
			}
			if len(m.dialog.password)+size > maxPasswordBytes {
				m.dialog.problem = "Password is too long; input was not added."
				return m, nil
			}
			if m.dialog.password == nil {
				// Reserve a trailing newline for sudo, avoiding secret-bearing
				// reallocations while typing. Clearing is best effort, not mlock.
				m.dialog.password = make([]byte, 0, maxPasswordBytes+1)
			}
			for _, r := range runes {
				m.dialog.password = utf8.AppendRune(m.dialog.password, r)
			}
			m.dialog.problem = ""
		}
		return m, nil
	}

	if msg.Paste || (msg.Type == tea.KeyRunes && len(msg.Runes) != 1) {
		return m, nil
	}
	switch msg.String() {
	case "tab", "shift+tab", "left", "right", "up", "down":
		m.dialog.confirm = !m.dialog.confirm
	case "esc", "n", "q", "ctrl+c":
		return m.cancelConfirmation()
	case "y":
		return m.acceptConfirmation()
	case "enter":
		if m.dialog.confirm {
			return m.acceptConfirmation()
		}
		return m.cancelConfirmation()
	}
	return m, nil
}

func (m *guiModel) cancelConfirmation() (tea.Model, tea.Cmd) {
	m.dismissDialog()
	// Resume a plan that arrived while the quit confirmation was open.
	if m.busy && m.plan != nil && m.session == nil && !m.starting {
		return m, m.continuePrepared()
	}
	return m, nil
}

func (m *guiModel) acceptConfirmation() (tea.Model, tea.Cmd) {
	if m.dialog.kind == dialogLANStop {
		return m.disconnectLAN()
	}
	quit := m.dialog.kind == dialogQuit
	m.dismissDialog()
	m.stopping = true
	m.status = "Stopping " + m.playing
	if quit {
		m.quitting, m.lan.stopping, m.lan.ready = true, true, false
		m.status = "Stopping the launch before quitting..."
	}
	if m.session != nil {
		m.session.cancel()
	} else if quit && !m.starting {
		// Preparation has no child to reap. In-flight starts, on the other
		// hand, must deliver startedMsg before shutdown can finish.
		m.busy, m.plan = false, nil
	}
	return m, m.advanceShutdown()
}

func dialogButton(text string, selected bool) string {
	style := lipgloss.NewStyle().Padding(0, 1)
	if selected {
		style = style.Bold(true).Reverse(true)
	}
	return style.Render("[" + text + "]")
}

func (m *guiModel) dialogContent(width int, compact bool) string {
	if m.dialog.kind == dialogLANMode || m.dialog.kind == dialogLANJoin || m.dialog.kind == dialogLANInfo {
		return m.lanDialogContent(width, compact)
	}
	line := func(text string) string { return clippedLine(text, width) }
	var title, reason, input, help string
	target := m.playing
	if m.dialog.kind == dialogPassword {
		title = "Sudo authentication"
		target = m.sudoPrompt.target
		reason = m.sudoPrompt.reason
		if compact {
			reason = "sudo is needed to configure or enter the network namespace."
		}
		input = "Password: " + strings.Repeat("*", min(utf8.RuneCount(m.dialog.password), max(0, width-11))) + "_"
		help = "enter: authenticate | esc: cancel"
		if m.auth != nil {
			input = "Authenticating..."
			if m.auth.probe {
				input = "Checking cached sudo credentials..."
			}
			help = "esc: cancel"
		}
	} else {
		title, reason = "Stop this game?", "This will stop the running game. Unsaved progress may be lost."
		button := "Stop game"
		if m.dialog.kind == dialogLANStop {
			title, target = "Disconnect this LAN?", "Network namespace: "+lanNamespace
			reason = "This stops lanparty and all games in the managed games namespace, then removes its veth pair and bridge. Unsaved progress may be lost."
			button = "Disconnect"
		}
		if m.dialog.kind == dialogQuit {
			title = "Stop and quit?"
			reason = "This will stop the active launch and close Heroic Wrapper. Unsaved progress may be lost."
			button = "Stop and quit"
			if m.lan.busy {
				reason = "This stops the game and lanparty, removes the managed games namespace, veth and bridge, and closes Heroic Wrapper. Unsaved progress may be lost."
				if !m.busy {
					target = "LAN connection / namespace: " + lanNamespace
				}
			}
		}
		input = dialogButton("Cancel", !m.dialog.confirm) + " " + dialogButton(button, m.dialog.confirm)
		help = "tab/arrows: choose | enter: accept | esc: cancel"
		if compact {
			input = dialogSlice(input, 0, width)
			help = "y: stop | n/esc: cancel"
		}
	}
	parts := []string{guiTitle.Render(line(title)), line(target), "", reason, "", input}
	if m.dialog.problem != "" {
		parts = append(parts, "", m.dialog.problem)
	}
	parts = append(parts, "", help)
	if compact {
		parts = []string{guiTitle.Render(line(title)), line(target), line(reason), input}
		if m.dialog.problem != "" {
			parts = append(parts, line(m.dialog.problem))
		}
		parts = append(parts, line(help))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(parts, "\n"))
}

func (m *guiModel) View() string {
	background := m.layoutView()
	if m.dialog.kind == dialogNone || m.width <= 0 || m.height <= 0 {
		return background
	}
	boxWidth := min(68, max(8, m.width-4))
	contentWidth := max(1, boxWidth-4)
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(guiAccent).Padding(0, 1)
	dialog := style.Render(m.dialogContent(contentWidth, false))
	if lipgloss.Height(dialog) > m.height-2 || m.width < 40 {
		dialog = style.Render(m.dialogContent(contentWidth, true))
	}
	return overlayDialog(background, dialog, m.width, m.height)
}

// Slice in terminal cells using the existing Bubbles viewport. It handles ANSI
// styles and wide Unicode characters without an extra compositing dependency.
func dialogSlice(line string, offset, width int) string {
	if width <= 0 {
		return ""
	}
	view := viewport.New(width, 1)
	view.SetContent(line + strings.Repeat(" ", max(0, offset+width-lipgloss.Width(line))))
	view.SetXOffset(offset)
	return view.View()
}

func overlayDialog(background, foreground string, width, height int) string {
	canvas := viewport.New(width, height)
	canvas.SetContent(background)
	rows := strings.Split(canvas.View(), "\n")
	w, h := min(width, lipgloss.Width(foreground)), min(height, lipgloss.Height(foreground))
	popup := viewport.New(w, h)
	popup.SetContent(foreground)
	x, y := (width-w)/2, (height-h)/2
	for i, row := range strings.Split(popup.View(), "\n") {
		base := rows[y+i]
		rows[y+i] = dialogSlice(base, 0, x) + row + dialogSlice(base, x+w, width-x-w)
	}
	return strings.Join(rows, "\n")
}
