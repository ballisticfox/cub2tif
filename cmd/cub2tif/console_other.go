//go:build !windows

package main

import (
	"os"

	"golang.org/x/term"
)

func launchedFromExplorer() bool { return false }

func enableVT() bool { return true }

// readKey reads one keypress without echo.
func readKey() key {
	fd := int(os.Stdin.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return key{kind: keyNone}
	}
	defer term.Restore(fd, st)
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return key{kind: keyQuit}
		}
		b := buf[:n]
		switch {
		case len(b) >= 3 && b[0] == 0x1b && (b[1] == '[' || b[1] == 'O'):
			switch b[2] {
			case 'A':
				return key{kind: keyUp}
			case 'B':
				return key{kind: keyDown}
			}
			// other escape sequences (left/right, function keys): ignore
		case b[0] == 0x1b, b[0] == 0x03, b[0] == 0x04: // Esc, Ctrl+C, Ctrl+D
			return key{kind: keyQuit}
		case b[0] == '\r' || b[0] == '\n':
			return key{kind: keyEnter}
		case b[0] == ' ':
			return key{kind: keySpace}
		default:
			return key{kind: keyRune, r: rune(b[0])}
		}
	}
}
