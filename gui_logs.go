package main

import (
	"strings"
	"sync"
	"unicode"
)

const gameLogCapacity = 128 * 1024

// gameLog is a bounded, concurrency-safe io.Writer shared by stdout and stderr.
// A byte ring avoids Scanner's line limit and unbounded memory use for output
// without newlines. Control sequences are removed before reaching the terminal.
// Rendering samples snapshots rather than sending one Bubble Tea message per line.
type gameLog struct {
	mu       sync.Mutex
	data     [gameLogCapacity]byte
	start    int
	length   int
	revision uint64
	dropped  bool
	escape   byte
	lastCR   bool
}

func (log *gameLog) Write(p []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, c := range p {
		switch log.escape {
		case 1: // ESC and intermediate escape bytes
			switch c {
			case '[':
				log.escape = 2
			case ']', 'P', '^', '_', 'X':
				log.escape = 3
			default:
				if c < 0x20 || c > 0x2f {
					log.escape = 0
				}
			}
			continue
		case 2: // CSI, including SGR colors and cursor/terminal commands
			if c >= 0x40 && c <= 0x7e {
				log.escape = 0
			}
			continue
		case 3: // OSC/DCS/APC/etc.; discard the entire payload
			if c == '\a' {
				log.escape = 0
			} else if c == 0x1b {
				log.escape = 4
			}
			continue
		case 4:
			if c == '\\' || c == '\a' {
				log.escape = 0
			} else if c != 0x1b {
				log.escape = 3
			}
			continue
		}
		if c == 0x1b {
			log.escape = 1
			continue
		}
		if c == '\n' && log.lastCR {
			log.lastCR = false
			continue
		}
		log.lastCR = c == '\r'
		if c == '\r' {
			c = '\n' // keep progress updates readable without terminal control
		}
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			continue
		}
		if log.length == len(log.data) {
			log.start = (log.start + 1) % len(log.data)
			log.length--
			log.dropped = true
		}
		log.data[(log.start+log.length)%len(log.data)] = c
		log.length++
	}
	log.revision++
	return len(p), nil
}

func (log *gameLog) snapshot(previous uint64) (string, uint64, bool) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if previous == log.revision {
		return "", previous, false
	}
	data := make([]byte, log.length)
	n := copy(data, log.data[log.start:min(log.start+log.length, len(log.data))])
	copy(data[n:], log.data[:log.length-n])
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
	if log.dropped {
		// Prefer a complete first line after wrapping; retain a newline-free
		// tail too, so a very long log line cannot make the panel go blank.
		if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline < len(text)-1 {
			text = text[newline+1:]
		}
		text = "[older output discarded; showing the latest 128 KiB]\n" + text
	}
	return text, log.revision, true
}

func displayLine(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, "\uFFFD"))
}
