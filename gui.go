package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/sys/unix"
)

const (
	focusGames = iota
	focusNamespaces
	focusLogs
)

type catalogMsg struct {
	games      []game
	namespaces []string
	gamesErr   error
	netnsErr   error
}
type preparedMsg struct {
	plan    *launchPlan
	command string
	err     error
}
type authenticatedMsg struct {
	attempt *sudoAttempt
	err     error
}
type startedMsg struct {
	session *gameSession
	err     error
}
type exitedMsg struct {
	session *gameSession
	err     error
}
type logTickMsg struct{ session *gameSession }

type guiModel struct {
	ctx         context.Context
	launcher    *gameLauncher
	games       []game
	matches     []int
	gameCursor  int
	namespaces  []string
	nsCursor    int
	focus       int
	width       int
	height      int
	query       string
	filtering   bool
	loading     bool
	busy        bool
	stopping    bool
	starting    bool
	dialog      guiDialog
	auth        *sudoAttempt
	quitting    bool
	status      string
	playing     string
	network     string
	plan        *launchPlan
	session     *gameSession
	output      *gameLog
	logView     viewport.Model
	logRevision uint64
	follow      bool
}

func guiTerminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

func runGUI() error {
	if !guiTerminal(os.Stdin) || !guiTerminal(os.Stdout) {
		return errors.New("the interactive UI requires a terminal; use list, show, or where for command-line output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	launcher := &gameLauncher{}
	defer func() {
		cancel()
		launcher.close()
	}()
	model := &guiModel{
		ctx: ctx, launcher: launcher, namespaces: []string{""},
		loading: true, status: "Loading installed games...", output: &gameLog{},
		logView: viewport.New(58, 5), follow: true,
	}
	defer model.dismissDialog()
	model.logView.SetHorizontalStep(4)
	model.logView.SetContent("Select a game, then a network namespace, and press enter to launch.")
	_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(),
		tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}

func loadCatalog() tea.Msg {
	games, gamesErr := allGames()
	namespaces, netnsErr := networkNamespaces()
	return catalogMsg{games, namespaces, gamesErr, netnsErr}
}

func (m *guiModel) Init() tea.Cmd { return loadCatalog }

func logTick(session *gameSession) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return logTickMsg{session} })
}

func waitForGame(session *gameSession) tea.Cmd {
	return func() tea.Msg {
		<-session.done
		<-session.settled
		return exitedMsg{session, session.err}
	}
}

func (m *guiModel) refreshFilter() {
	m.matches = m.matches[:0]
	query := strings.ToLower(m.query)
	for i, game := range m.games {
		if strings.Contains(strings.ToLower(game.title+" "+game.store+":"+game.id), query) {
			m.matches = append(m.matches, i)
		}
	}
	m.gameCursor = min(m.gameCursor, max(0, len(m.matches)-1))
}

func (m *guiModel) syncLogs() {
	text, revision, changed := m.output.snapshot(m.logRevision)
	if !changed {
		return
	}
	m.logRevision = revision
	m.logView.SetContent(text)
	if m.follow {
		m.logView.GotoBottom()
	}
}

func (m *guiModel) launchError(err error) (tea.Model, tea.Cmd) {
	m.dismissDialog()
	m.busy, m.starting, m.plan = false, false, nil
	m.status = "Launch failed: " + err.Error()
	fmt.Fprintln(m.output, "\n[heroic-wrapper]", m.status)
	m.syncLogs()
	if m.quitting {
		return m, tea.Quit
	}
	return m, nil
}

func (m *guiModel) startPrepared() tea.Cmd {
	ctx, launcher, plan, output := m.ctx, m.launcher, m.plan, m.output
	m.starting = true
	m.status = "Starting " + m.playing
	return func() tea.Msg {
		session, err := launcher.start(ctx, plan, output)
		return startedMsg{session, err}
	}
}

func (m *guiModel) beginLaunch() tea.Cmd {
	if m.loading || m.busy || len(m.matches) == 0 {
		return nil
	}
	chosen := m.games[m.matches[m.gameCursor]]
	namespace := m.namespaces[m.nsCursor]
	m.playing = displayLine(chosen.title) + " / " + namespaceLabel(namespace)
	m.network = namespace
	m.dismissDialog()
	m.busy, m.stopping, m.starting, m.plan = true, false, false, nil
	m.output, m.logRevision, m.follow = &gameLog{}, 0, true
	m.logView.SetContent("")
	m.logView.GotoTop()
	m.logView.SetXOffset(0)
	fmt.Fprintln(m.output, "[heroic-wrapper]", m.playing)
	m.syncLogs()
	m.status = "Preparing " + m.playing
	return func() tea.Msg {
		// Use exactly the same discovery/configuration/Nix/Comet logic as
		// `show`, without matching the selected title a second time.
		command, err := buildCommand(chosen, false, false)
		if err != nil {
			return preparedMsg{err: err}
		}
		plan, err := prepareLaunch(command, namespace)
		return preparedMsg{plan, command, err}
	}
}

func (m *guiModel) requestQuit() (tea.Model, tea.Cmd) {
	if !m.busy {
		return m, tea.Quit
	}
	m.dialog = guiDialog{kind: dialogQuit}
	return m, nil
}

func (m *guiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.logView.Width = max(1, m.width-2)
		m.logView.Height = max(1, m.height-m.selectorRows()-10)
		if m.follow {
			m.logView.GotoBottom()
		}
		return m, nil
	case catalogMsg:
		m.loading = false
		m.games, m.namespaces = msg.games, msg.namespaces
		m.nsCursor, m.gameCursor = 0, 0
		m.refreshFilter()
		m.status = fmt.Sprintf("%d installed games. Select a game, then a network namespace.", len(m.games))
		if msg.gamesErr != nil {
			m.status = msg.gamesErr.Error() + " (r to retry)"
		} else if len(m.games) == 0 {
			m.status = "No installed games found. Check HEROIC_CONFIG_DIR; r to retry."
		}
		if msg.netnsErr != nil {
			m.status += " | " + msg.netnsErr.Error()
		}
		return m, nil
	case preparedMsg:
		if m.quitting {
			return m, tea.Quit
		}
		if msg.err != nil {
			return m.launchError(msg.err)
		}
		m.plan = msg.plan
		// Show generator notes, but do not dump commands containing secrets.
		for _, line := range strings.Split(msg.command, "\n") {
			if strings.HasPrefix(line, "# ") {
				fmt.Fprintln(m.output, line)
			}
		}
		m.syncLogs()
		// A quit confirmation can open while buildCommand is still working.
		// Keep that dialog in front and defer authentication/start until cancel.
		if m.dialog.kind == dialogQuit {
			return m, nil
		}
		return m, m.continuePrepared()
	case authenticatedMsg:
		// Ignore a late result after cancellation or a subsequent launch.
		if msg.attempt == nil || msg.attempt != m.auth {
			return m, nil
		}
		m.auth = nil
		if msg.err != nil {
			m.status = "Sudo authentication required; enter a password or cancel."
			if !msg.attempt.probe {
				m.dialog.problem = msg.err.Error()
			}
			return m, nil
		}
		m.dismissDialog()
		return m, m.startPrepared()
	case startedMsg:
		m.starting = false
		if msg.err != nil {
			return m.launchError(msg.err)
		}
		m.session, m.focus = msg.session, focusLogs
		m.status = "Running " + m.playing
		if m.quitting {
			m.status = "Stopping the launch before quitting..."
			m.session.cancel()
		}
		return m, tea.Batch(waitForGame(m.session), logTick(m.session))
	case logTickMsg:
		if msg.session == m.session {
			m.syncLogs()
			return m, logTick(m.session)
		}
		return m, nil
	case exitedMsg:
		if msg.session != m.session {
			return m, nil
		}
		m.busy, m.session, m.plan = false, nil, nil
		// The game may finish naturally while a confirmation is visible.
		m.dismissDialog()
		m.status = "Finished " + m.playing
		if m.stopping {
			m.status = "Stopped " + m.playing
		} else if msg.err != nil {
			m.status = "Game exited: " + msg.err.Error()
		}
		fmt.Fprintln(m.output, "\n[heroic-wrapper]", m.status)
		m.syncLogs()
		if m.quitting {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyMsg:
		if m.dialog.kind != dialogNone {
			return m.updateDialog(msg)
		}
		if m.quitting {
			return m, nil
		}
		key := msg.String()
		if key == "ctrl+c" {
			return m.requestQuit()
		}
		if m.filtering {
			switch key {
			case "esc", "enter":
				m.filtering = false
			case "backspace", "ctrl+h":
				_, size := utf8.DecodeLastRuneInString(m.query)
				m.query = m.query[:len(m.query)-size]
			case "ctrl+u":
				m.query = ""
			default:
				if msg.Type == tea.KeyRunes && utf8.RuneCountInString(m.query)+len(msg.Runes) <= 128 {
					m.query += string(msg.Runes)
				}
			}
			m.gameCursor = 0
			m.refreshFilter()
			return m, nil
		}
		if key == "q" {
			return m.requestQuit()
		}
		switch key {
		case "tab":
			m.focus = (m.focus + 1) % 3
		case "shift+tab":
			m.focus = (m.focus + 2) % 3
		case "esc":
			m.focus = focusGames
			if m.busy {
				m.status = "Running " + m.playing
			}
		case "/":
			m.focus, m.filtering = focusGames, true
		case "r":
			if !m.busy && !m.loading {
				m.loading, m.status = true, "Refreshing games and namespaces..."
				return m, loadCatalog
			}
		case "s":
			if m.session != nil && !m.stopping {
				m.dialog = guiDialog{kind: dialogStop}
			}
		case "f":
			m.focus, m.follow = focusLogs, true
			m.logView.GotoBottom()
		case "enter":
			if m.width < 60 || m.height < 20 {
				return m, nil
			}
			if m.focus == focusGames && len(m.matches) > 0 {
				m.focus = focusNamespaces
			} else if m.focus == focusNamespaces {
				return m, m.beginLaunch()
			}
		default:
			if m.focus == focusLogs {
				var cmd tea.Cmd
				m.logView, cmd = m.logView.Update(msg)
				m.follow = m.logView.AtBottom()
				return m, cmd
			}
			cursor, count := &m.gameCursor, len(m.matches)
			if m.focus == focusNamespaces {
				cursor, count = &m.nsCursor, len(m.namespaces)
			}
			switch key {
			case "up", "k":
				*cursor = max(0, *cursor-1)
			case "down", "j":
				*cursor = min(max(0, count-1), *cursor+1)
			case "home", "g":
				*cursor = 0
			case "end", "G":
				*cursor = max(0, count-1)
			case "pgup":
				*cursor = max(0, *cursor-m.selectorRows())
			case "pgdown":
				*cursor = min(max(0, count-1), *cursor+m.selectorRows())
			}
		}
		return m, nil
	case tea.MouseMsg:
		if m.dialog.kind != dialogNone || m.quitting {
			return m, nil
		}
		if m.focus == focusLogs {
			var cmd tea.Cmd
			m.logView, cmd = m.logView.Update(msg)
			m.follow = m.logView.AtBottom()
			return m, cmd
		}
	}
	return m, nil
}

func namespaceLabel(name string) string {
	if name == "" {
		return "Current network namespace"
	}
	return displayLine(name)
}

func (m *guiModel) selectorRows() int { return max(4, min(9, m.height/4)) }

var (
	guiAccent = lipgloss.Color("75")
	guiTitle  = lipgloss.NewStyle().Bold(true).Foreground(guiAccent)
)

func clippedLine(text string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).MaxHeight(1).Render(displayLine(text))
}

func choiceRows(labels []string, cursor, height, width int) string {
	start := max(0, min(cursor-height/2, len(labels)-height))
	rows := make([]string, height)
	for row := range rows {
		index := start + row
		if index >= len(labels) {
			continue
		}
		prefix := "  "
		if index == cursor {
			prefix = "> "
		}
		rows[row] = clippedLine(prefix+labels[index], width)
		if index == cursor {
			rows[row] = guiTitle.Render(rows[row])
		}
	}
	if len(labels) == 0 {
		rows[0] = "  (no matches)"
	}
	return strings.Join(rows, "\n")
}

func guiPanel(title, content string, width int, focused bool) string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(width)
	if focused {
		style = style.BorderForeground(guiAccent)
	}
	return style.Render(guiTitle.Render(clippedLine(title, width)) + "\n" + content)
}

func (m *guiModel) layoutView() string {
	if m.width == 0 {
		return "Opening Heroic Wrapper..."
	}
	if m.width < 60 || m.height < 20 {
		return "Resize the terminal to at least 60 columns and 20 rows.\nq/ctrl+c: quit (confirmation during a launch)."
	}
	gameWidth := m.width*2/3 - 2
	namespaceWidth := m.width - gameWidth - 4
	gameLabels := make([]string, len(m.matches))
	for i, index := range m.matches {
		game := m.games[index]
		gameLabels[i] = game.title + " [" + game.store + ":" + game.id + "]"
	}
	namespaceLabels := make([]string, len(m.namespaces))
	for i, name := range m.namespaces {
		namespaceLabels[i] = namespaceLabel(name)
	}
	rows := m.selectorRows()
	games := guiPanel(fmt.Sprintf("Games (%d)", len(m.matches)),
		choiceRows(gameLabels, m.gameCursor, rows, gameWidth), gameWidth, m.focus == focusGames)
	namespaces := guiPanel("Network namespace",
		choiceRows(namespaceLabels, m.nsCursor, rows, namespaceWidth), namespaceWidth, m.focus == focusNamespaces)
	filter := "Filter: " + m.query + "  (/ to edit)"
	if m.filtering {
		filter = "Filter: " + m.query + "_  (enter/esc to finish)"
	}
	logTitle := "Game logs (stdout + stderr)"
	if m.follow {
		logTitle += " - following"
	} else {
		logTitle += " - scroll paused; f to follow"
	}
	help := "tab: focus | /: filter | enter: select/launch | r: refresh | q: quit"
	if m.focus == focusLogs {
		help = "arrows/pgup/pgdown: scroll | f: follow | s: stop | tab: focus | q: quit"
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		guiTitle.Render("Heroic Wrapper"), clippedLine(filter, m.width),
		lipgloss.JoinHorizontal(lipgloss.Top, games, namespaces),
		guiPanel(logTitle, m.logView.View(), m.width-2, m.focus == focusLogs),
		clippedLine(m.status, m.width), clippedLine(help, m.width))
}
