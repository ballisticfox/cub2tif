package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"cub2tif/internal/raster"
)

// Console primitives for the wizard. Every prompt treats end of input
// (Ctrl+Z, closed stdin) as "quit" rather than looping forever, and every
// arrow-key widget falls back to typed input on a console it can't drive.

var stdin = bufio.NewReader(os.Stdin)

func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// pauseIfNeeded holds the window open after a double-click or drag-and-drop
// run, which would otherwise close the console and take the summary with it.
// A console the user typed into stays theirs: no pause there.
func pauseIfNeeded(args []string) {
	for _, a := range args {
		if a == "--no-pause" || a == "-no-pause" {
			return
		}
	}
	if !interactive() || !launchedFromExplorer() {
		return
	}
	fmt.Println()
	fmt.Println("  Press any key to close.")
	if k := readKey(); k.kind == keyNone {
		stdin.ReadString('\n')
	}
}

type keyKind int

const (
	keyNone keyKind = iota // raw input unavailable
	keyUp
	keyDown
	keyEnter
	keySpace
	keyQuit
	keyRune
)

type key struct {
	kind keyKind
	r    rune
}

// prompt reads one line. ok is false at end of input.
func prompt() (string, bool) {
	fmt.Print("  > ")
	s, err := stdin.ReadString('\n')
	if err != nil && s == "" {
		fmt.Println()
		return "", false
	}
	return strings.TrimSpace(s), true
}

func warn(msg string) { fmt.Printf("  ! %s\n", msg) }

func rule() { fmt.Println("  " + strings.Repeat("-", 66)) }

func confirm(label string, def bool) bool {
	yn := "y/N"
	if def {
		yn = "Y/n"
	}
	for {
		fmt.Printf("%s [%s] ", label, yn)
		s, err := stdin.ReadString('\n')
		if err != nil && s == "" {
			fmt.Println()
			return def
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch {
		case s == "":
			return def
		case strings.HasPrefix(s, "y"):
			return true
		case strings.HasPrefix(s, "n"):
			return false
		}
	}
}

// askLine asks for free text with a default shown in brackets.
func askLine(label, def string) (string, bool) {
	if def != "" {
		fmt.Printf("%s [%s] ", label, def)
	} else {
		fmt.Printf("%s ", label)
	}
	s, err := stdin.ReadString('\n')
	if err != nil && s == "" {
		fmt.Println()
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, true
	}
	return s, true
}

func askInt(label string, def, lo, hi int) int {
	for {
		s, ok := askLine(label, strconv.Itoa(def))
		if !ok {
			return def
		}
		if v, err := strconv.Atoi(s); err == nil && v >= lo && v <= hi {
			return v
		}
		warn(fmt.Sprintf("Enter a whole number between %d and %d.", lo, hi))
	}
}

// askFloat returns def with changed=false when the user just presses enter.
func askFloat(label string, def float64, defText string, lo, hi float64) (v float64, changed, ok bool) {
	for {
		s, ok := askLine(label, defText)
		if !ok {
			return def, false, false
		}
		if s == defText {
			return def, false, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err == nil && f >= lo && f <= hi {
			return f, true, true
		}
		warn(fmt.Sprintf("Enter a number between %s and %s.", raster.FormatNum(lo), raster.FormatNum(hi)))
	}
}

// menuItem is one row of a selection list.
type menuItem struct {
	label  string
	detail string
}

// selectOne is an arrow-key list. Returns the chosen index, or -1 on quit.
// Falls back to a numbered prompt on consoles that can't redraw in place.
func selectOne(title string, items []menuItem, def int) int {
	fmt.Printf("  %s   [up/down] move   [enter] select   [q] quit\n\n", title)
	if !enableVT() {
		return selectTyped(items, def)
	}
	cursor := def
	width := 0
	for _, it := range items {
		width = max(width, len(it.label))
	}
	draw := func(redraw bool) {
		if redraw {
			fmt.Printf("\x1b[%dA", len(items))
		}
		for i, it := range items {
			mark := "  "
			if i == cursor {
				mark = "> "
			}
			line := fmt.Sprintf("      %s%-*s   %s", mark, width, it.label, it.detail)
			if i == cursor {
				line = "\x1b[1m" + line + "\x1b[0m"
			}
			fmt.Printf("\r\x1b[2K%s\n", line)
		}
	}
	draw(false)
	for {
		k := readKey()
		switch k.kind {
		case keyNone:
			return selectTyped(items, def)
		case keyUp:
			cursor = (cursor - 1 + len(items)) % len(items)
		case keyDown:
			cursor = (cursor + 1) % len(items)
		case keyEnter, keySpace:
			fmt.Println()
			return cursor
		case keyQuit:
			return -1
		case keyRune:
			if k.r == 'q' || k.r == 'Q' {
				return -1
			}
			if k.r >= '1' && k.r <= '9' && int(k.r-'1') < len(items) {
				cursor = int(k.r - '1')
			}
		}
		draw(true)
	}
}

func selectTyped(items []menuItem, def int) int {
	for i, it := range items {
		fmt.Printf("      [%d] %s   %s\n", i+1, it.label, it.detail)
	}
	fmt.Println()
	for {
		s, ok := askLine("      Which?", strconv.Itoa(def+1))
		if !ok || strings.EqualFold(s, "q") {
			return -1
		}
		if v, err := strconv.Atoi(s); err == nil && v >= 1 && v <= len(items) {
			fmt.Println()
			return v - 1
		}
		warn(fmt.Sprintf("Enter a number between 1 and %d.", len(items)))
	}
}

// actionKey waits for enter or one of the given letters and returns "enter",
// the letter, or "" for quit. Without raw input it reads a line instead.
func actionKey(letters string) string {
	for {
		k := readKey()
		switch k.kind {
		case keyNone:
			s, ok := prompt()
			if !ok {
				return ""
			}
			s = strings.ToLower(s)
			if s == "" {
				return "enter"
			}
			if s[:1] == "q" {
				return ""
			}
			if strings.Contains(letters, s[:1]) {
				return s[:1]
			}
		case keyEnter:
			return "enter"
		case keyQuit:
			return ""
		case keyRune:
			c := strings.ToLower(string(k.r))
			if c == "q" {
				return ""
			}
			if strings.Contains(letters, c) {
				return c
			}
		}
	}
}

// cleanPath normalises the shapes Windows hands us: quoted by "Copy as path",
// or with a trailing separator from a drag. A drive root keeps its separator,
// where "E:\" and "E:" mean different directories.
func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if len(s) > 1 && !strings.HasSuffix(s, ":\\") && !strings.HasSuffix(s, ":/") {
		s = strings.TrimRight(s, "\\/")
	}
	return s
}

// splitPaths splits a line holding one or more paths, as produced by dropping
// several files onto the console: space separated, quoted when they contain
// spaces. A single unquoted path with spaces that exists is kept whole.
func splitPaths(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	if _, err := os.Stat(cleanPath(line)); err == nil {
		return []string{cleanPath(line)}
	}
	var out []string
	var cur strings.Builder
	quote := byte(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cleanPath(cur.String()))
			cur.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && (c == ' ' || c == '\t'):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// shellQuote quotes an argument for the echoed command line when needed.
func shellQuote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t&()[]{}^=;!'+,`~%$\"|<>") {
		return `"` + s + `"`
	}
	return s
}
