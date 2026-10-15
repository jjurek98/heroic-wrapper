package main

import (
	"context"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Each popup has a distinct identity so a late clipboard result cannot update
// a different dialog, even if the same server token has been opened again.
type tokenDialogState struct {
	token  string
	busy   bool
	cancel context.CancelFunc
}

type tokenCopiedMsg struct {
	dialog *tokenDialogState
	notice string
	err    error
}

type tokenRawClosedMsg struct {
	dialog *tokenDialogState
	err    error
}

func (m *guiModel) showLANToken() {
	m.dismissDialog()
	m.dialog = guiDialog{
		kind: dialogLANToken, title: "LAN server token", text: m.lan.token,
		token: &tokenDialogState{token: m.lan.token},
	}
}

func (m *guiModel) updateTokenDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste {
		return m, nil
	}
	state := m.dialog.token
	switch msg.String() {
	case "enter", "esc", "q", "ctrl+c":
		m.dismissDialog()
	case "c":
		if state.busy {
			return m, nil
		}
		ctx, cancel := context.WithCancel(m.ctx)
		state.busy, state.cancel = true, cancel
		m.dialog.problem = "Copying token..."
		token := state.token
		return m, func() tea.Msg {
			defer cancel()
			notice, err := copyLANToken(ctx, token, os.Stdout)
			return tokenCopiedMsg{state, notice, err}
		}
	case "r":
		if state.busy {
			return m, nil
		}
		ctx, cancel := context.WithCancel(m.ctx)
		state.busy, state.cancel, m.dialog.problem = true, cancel, ""
		// Exec suspends rendering/input, leaves the alternate screen and
		// disables mouse reporting. No shell or secret-bearing argv is needed.
		return m, tea.Exec(&rawTokenView{ctx: ctx, token: state.token}, func(err error) tea.Msg {
			defer cancel()
			return tokenRawClosedMsg{state, err}
		})
	}
	return m, nil
}

func (m *guiModel) tokenCopied(msg tokenCopiedMsg) (tea.Model, tea.Cmd) {
	if m.dialog.kind == dialogLANToken && m.dialog.token == msg.dialog {
		msg.dialog.busy, msg.dialog.cancel = false, nil
		m.dialog.problem = msg.notice
		if msg.err != nil {
			m.dialog.problem = "Copy failed: " + msg.err.Error() + ". Press r to copy manually."
		}
	}
	return m, nil
}

func (m *guiModel) tokenRawClosed(msg tokenRawClosedMsg) (tea.Model, tea.Cmd) {
	if m.dialog.kind == dialogLANToken && m.dialog.token == msg.dialog {
		msg.dialog.busy, msg.dialog.cancel = false, nil
		if msg.err != nil {
			m.dialog.problem = "Raw token view: " + msg.err.Error()
		}
	}
	// Bubble Tea v1 restores the alternate screen and redraws after Exec,
	// but does not re-enable mouse tracking. Restore it even for stale replies.
	return m, tea.EnableMouseCellMotion
}
