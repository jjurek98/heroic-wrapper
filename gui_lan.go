package main

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type guiLAN struct {
	launcher *lanLauncher
	plan     *lanPlan
	session  *lanSession
	busy     bool
	starting bool
	stopping bool
	ready    bool
	status   string
	token    string
	output   *gameLog
	view     viewport.Model
	revision uint64
	follow   bool
}

type lanStartedMsg struct {
	session *lanSession
	err     error
}

type lanTickMsg struct{ session *lanSession }

func newGUILAN() guiLAN {
	view := viewport.New(58, 5)
	view.SetHorizontalStep(4)
	view.SetContent("Press l to host a LAN or join a server with its token. lanparty must already be installed.")
	return guiLAN{launcher: &lanLauncher{}, output: &gameLog{}, view: view, follow: true, status: "disconnected"}
}

func (m *guiModel) activeLogView() *viewport.Model {
	if m.lanLogs {
		return &m.lan.view
	}
	return &m.logView
}

func (m *guiModel) activeLogFollow() *bool {
	if m.lanLogs {
		return &m.lan.follow
	}
	return &m.follow
}

func (m *guiModel) syncLANLogs() {
	text, revision, changed := m.lan.output.snapshot(m.lan.revision)
	if changed {
		m.lan.revision = revision
		m.lan.view.SetContent(text)
		if m.lan.follow {
			m.lan.view.GotoBottom()
		}
	}
}

func (m *guiModel) showLANMessage(title, text string) {
	// A helper failure can arrive while a game is asking for sudo. Cancel
	// that request rather than allowing its late reply to start a stale plan.
	if m.dialog.kind == dialogPassword && m.sudoPrompt != nil {
		abort := m.sudoPrompt.abort
		m.dismissDialog()
		abort()
	} else {
		m.dismissDialog()
	}
	log := &gameLog{}
	_, _ = log.Write([]byte(text))
	safe, _, _ := log.snapshot(0)
	m.dialog = guiDialog{kind: dialogLANInfo, title: title, text: safe}
}

func (m *guiModel) openLAN() (tea.Model, tea.Cmd) {
	if m.busy && m.session == nil {
		m.status = "Finish or cancel the current game launch before changing LAN settings."
		return m, nil
	}
	if m.lan.busy {
		if !m.lan.stopping {
			m.dialog = guiDialog{kind: dialogLANStop}
		}
		return m, nil
	}
	if _, err := exec.LookPath("lanparty"); err != nil {
		m.showLANMessage("lanparty is not installed", errLANNotInstalled.Error())
		return m, nil
	}
	m.dialog = guiDialog{kind: dialogLANMode}
	return m, nil
}

func (m *guiModel) beginLAN(server bool, token string) (tea.Model, tea.Cmd) {
	plan, err := prepareLAN(server, token)
	if err != nil {
		m.showLANMessage("Cannot start LAN", err.Error())
		return m, nil
	}
	m.dismissDialog()
	m.lan.plan, m.lan.busy, m.lan.ready = plan, true, false
	m.lan.stopping, m.lan.starting, m.lan.token = false, false, ""
	m.lan.output, m.lan.revision, m.lan.follow = &gameLog{}, 0, true
	m.lan.view.SetContent("")
	m.lan.view.GotoTop()
	m.lan.view.SetXOffset(0)
	m.lanLogs, m.focus = true, focusLogs
	mode := "Hosting a LAN"
	if !server {
		mode = "Joining a LAN"
	}
	m.lan.status = "waiting for sudo authorization"
	fmt.Fprintln(m.lan.output, "[heroic-wrapper]", mode)
	m.syncLANLogs()
	return m, m.requestSudo(plan.sudo, mode+" / namespace: "+lanNamespace,
		"Administrator permission is needed to run the installed lanparty tool, create its TAP interface, and configure games, br-games and a veth pair. Games still run as your normal user. These network resources will be removed on disconnect or exit.",
		m.startPreparedLAN, func() {
			m.lan.busy, m.lan.plan = false, nil
			m.lan.status = "connection canceled"
		})
}

func (m *guiModel) startPreparedLAN() tea.Cmd {
	ctx, launcher, plan, output := m.ctx, m.lan.launcher, m.lan.plan, m.lan.output
	m.lan.starting = true
	m.lan.status = "starting lanparty and waiting for its interface"
	return func() tea.Msg {
		session, err := launcher.start(ctx, plan, output)
		return lanStartedMsg{session, err}
	}
}

func lanTick(session *lanSession) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return lanTickMsg{session} })
}

func (m *guiModel) selectLANNamespace() {
	for i, name := range m.namespaces {
		if name == lanNamespace {
			m.nsCursor = i
			return
		}
	}
	m.namespaces = append(m.namespaces, lanNamespace)
	m.nsCursor = len(m.namespaces) - 1
}

func (m *guiModel) removeLANNamespace() {
	// Refresh only namespaces, not the game selection/filter. A conflict with
	// pre-existing "games" must not hide or remove that namespace from the UI.
	selected := ""
	if m.nsCursor < len(m.namespaces) {
		selected = m.namespaces[m.nsCursor]
	}
	if namespaces, err := networkNamespaces(); err == nil {
		m.namespaces, m.nsCursor = namespaces, 0
		for i, name := range namespaces {
			if name == selected {
				m.nsCursor = i
			}
		}
	}
}

func (m *guiModel) updateLANStarted(msg lanStartedMsg) (tea.Model, tea.Cmd) {
	m.lan.starting, m.lan.plan = false, nil
	if msg.err != nil {
		m.lan.busy, m.lan.ready = false, false
		m.lan.status = "start failed"
		fmt.Fprintln(m.lan.output, "[heroic-wrapper]", msg.err)
		m.syncLANLogs()
		if !m.quitting {
			m.showLANMessage("LAN could not start", msg.err.Error())
		}
		return m, m.advanceShutdown()
	}
	m.lan.session = msg.session
	if m.lan.stopping || m.quitting {
		m.lan.stopping = true
		m.advanceShutdown()
	}
	return m, lanTick(msg.session)
}

func (m *guiModel) updateLANTick(msg lanTickMsg) (tea.Model, tea.Cmd) {
	if msg.session != m.lan.session {
		return m, nil
	}
	// Lifecycle events are independent of the bounded text log: a token or
	// readiness notification cannot be lost when verbose logs wrap the ring.
	for {
		select {
		case event, ok := <-msg.session.events:
			if !ok {
				goto drained
			}
			switch event.Kind {
			case "offline":
				m.lan.ready, m.lan.token = false, ""
				m.lan.status = "connection lost; l to disconnect"
				m.cancelPendingLANGame()
				if !m.quitting && !m.lan.stopping {
					m.showLANMessage("LAN connection lost", event.Text)
				}
			case "token":
				m.lan.token = event.Token
			case "ready":
				if !m.lan.stopping && !m.quitting {
					m.lan.ready = true
					m.lan.status = fmt.Sprintf("connected: %s / %s / %s", event.Interface, event.CIDR, lanNamespace)
					m.selectLANNamespace()
				}
			}
		default:
			goto drained
		}
	}
drained:
	m.syncLANLogs()
	select {
	case <-msg.session.done:
		m.lan.busy, m.lan.ready, m.lan.session = false, false, nil
		m.lan.status, m.lan.token = "disconnected", ""
		m.cancelPendingLANGame()
		m.removeLANNamespace()
		if m.dialog.kind == dialogLANStop || (m.dialog.kind == dialogQuit && !m.busy) {
			m.dismissDialog()
		}
		if msg.session.err != nil {
			// Do not silently close the UI over a failed cleanup. Keep the
			// detailed diagnostics in the LAN log for recovery by the user.
			m.quitting = false
			m.lan.status = "stopped with errors; inspect LAN logs"
			m.showLANMessage("LAN stopped with errors", msg.session.err.Error()+"\nCheck the LAN log; any failed cleanup commands are reported there.")
		}
		return m, m.advanceShutdown()
	default:
		return m, lanTick(msg.session)
	}
}

func (m *guiModel) cancelPendingLANGame() {
	if m.gameLAN && m.busy && m.session == nil && !m.starting {
		// Invalidate a late buildCommand result after loss of its LAN.
		m.launchID++
		m.busy, m.plan = false, nil
		if m.dialog.kind == dialogPassword {
			m.dismissDialog()
		}
		m.status = "Launch canceled: LAN disconnected."
	}
}

// Game shutdown precedes network shutdown. The helper also reaps any remaining
// processes in its owned namespace, including games started outside this TUI.
func (m *guiModel) advanceShutdown() tea.Cmd {
	if m.quitting && m.busy {
		return nil
	}
	if m.lan.stopping && (!m.busy || m.network != lanNamespace) {
		m.lan.ready = false
		if m.lan.session != nil {
			m.lan.status = "disconnecting and removing network resources"
			m.lan.session.cancel()
		} else if !m.lan.starting {
			m.lan.busy, m.lan.plan = false, nil
		}
	}
	if m.quitting && !m.busy && !m.lan.busy {
		return tea.Quit
	}
	return nil
}

func (m *guiModel) disconnectLAN() (tea.Model, tea.Cmd) {
	m.dismissDialog()
	m.lan.stopping, m.lan.ready = true, false
	if m.busy && m.network == lanNamespace && m.session != nil {
		m.stopping = true
		m.session.cancel()
	}
	return m, m.advanceShutdown()
}

func (m *guiModel) updateLANDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dialog.kind == dialogLANJoin {
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.dismissDialog()
		case tea.KeyEnter, tea.KeyCtrlJ:
			token := strings.TrimSpace(m.dialog.text)
			if err := validateLANToken(token); err != nil {
				m.dialog.problem = err.Error()
				return m, nil
			}
			return m.beginLAN(false, token)
		case tea.KeyBackspace, tea.KeyCtrlH:
			_, size := utf8.DecodeLastRuneInString(m.dialog.text)
			m.dialog.text = m.dialog.text[:len(m.dialog.text)-size]
		case tea.KeyCtrlU:
			m.dialog.text = ""
		case tea.KeyRunes, tea.KeySpace:
			if msg.Alt {
				return m, nil
			}
			value := string(msg.Runes)
			if msg.Type == tea.KeySpace {
				value = " "
			}
			if msg.Paste {
				value = strings.TrimSpace(value)
			}
			for _, r := range value {
				if unicode.IsControl(r) {
					m.dialog.problem = "Paste a single-line server token."
					return m, nil
				}
			}
			if len(m.dialog.text)+len(value) <= lanTokenLimit {
				m.dialog.text += value
				m.dialog.problem = ""
			} else {
				m.dialog.problem = "Token is too long; input was not added."
			}
		}
		return m, nil
	}
	if msg.Paste {
		return m, nil
	}
	if m.dialog.kind == dialogLANInfo {
		switch msg.String() {
		case "enter", "esc", "q", "ctrl+c":
			m.dismissDialog()
		}
		return m, nil
	}
	// LAN mode selection; stop confirmation uses the shared confirmation UI.
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.dismissDialog()
	case "tab", "shift+tab", "left", "right", "up", "down":
		m.dialog.confirm = !m.dialog.confirm
	case "enter":
		if m.dialog.confirm {
			m.dialog = guiDialog{kind: dialogLANJoin}
			return m, nil
		}
		return m.beginLAN(true, "")
	}
	return m, nil
}

func (m *guiModel) lanDialogContent(width int, compact bool) string {
	var title, body, input, help string
	switch m.dialog.kind {
	case dialogLANMode:
		title = "Create or join a LAN?"
		body = "Run the installed lanparty tool as a server, or connect to a friend's server using their token."
		input = dialogButton("Server", !m.dialog.confirm) + " " + dialogButton("Join", m.dialog.confirm)
		help = "tab/arrows: choose | enter: continue | esc: cancel"
	case dialogLANJoin:
		title, body = "Join a LAN", "Paste the complete token shared by the server."
		input = "Token: " + m.dialog.text + "_"
		if lipgloss.Width(input) > width {
			input = "Token: ..." + dialogSlice(m.dialog.text+"_", max(0, lipgloss.Width(m.dialog.text+"_")-width+10), max(1, width-10))
		}
		help = "enter: connect | ctrl+u: clear | esc: cancel"
	case dialogLANInfo:
		title, body = m.dialog.title, m.dialog.text
		help = "enter/esc: close"
	}
	parts := []string{guiTitle.Render(clippedLine(title, width)), "", body, "", input}
	if m.dialog.problem != "" {
		parts = append(parts, "", m.dialog.problem)
	}
	parts = append(parts, "", help)
	if compact {
		parts = []string{guiTitle.Render(clippedLine(title, width)), clippedLine(body, width), dialogSlice(input, 0, width), clippedLine(m.dialog.problem, width), clippedLine(help, width)}
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(parts, "\n"))
}
