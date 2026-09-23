package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcess = kernel32.NewProc("GetConsoleProcessList")
	procGetConsoleMode    = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode    = kernel32.NewProc("SetConsoleMode")
)

// launchedFromExplorer reports whether this process owns its console window,
// i.e. it was started by double-clicking or by dropping files onto the exe.
// A console typed into by the user has the shell attached as well.
func launchedFromExplorer() bool {
	if procGetConsoleProcess.Find() != nil {
		return false
	}
	var pids [4]uint32
	n, _, _ := procGetConsoleProcess.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

var procReadConsoleInput = kernel32.NewProc("ReadConsoleInputW")

// inputRecord mirrors INPUT_RECORD holding a KEY_EVENT_RECORD.
type inputRecord struct {
	eventType uint16
	_         uint16
	keyDown   int32
	repeat    uint16
	vk        uint16
	scan      uint16
	char      uint16
	ctrl      uint32
}

// readKey reads one keypress from the console's event queue, the way .NET's
// Console.ReadKey does. Reading key events rather than bytes is what makes
// arrow keys work in every Windows console host, ConPTY included.
func readKey() key {
	h := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return key{kind: keyNone}
	}
	for {
		var rec inputRecord
		var n uint32
		r, _, _ := procReadConsoleInput.Call(uintptr(h), uintptr(unsafe.Pointer(&rec)), 1, uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return key{kind: keyNone}
		}
		if n == 0 || rec.eventType != 1 || rec.keyDown == 0 { // KEY_EVENT, key down only
			continue
		}
		switch rec.vk {
		case 0x26: // VK_UP
			return key{kind: keyUp}
		case 0x28: // VK_DOWN
			return key{kind: keyDown}
		case 0x0D: // VK_RETURN
			return key{kind: keyEnter}
		case 0x1B: // VK_ESCAPE
			return key{kind: keyQuit}
		case 0x20: // VK_SPACE
			return key{kind: keySpace}
		}
		switch rec.char {
		case 0:
			continue // shift, ctrl, other non-character keys
		case 0x03, 0x1a: // Ctrl+C, Ctrl+Z
			return key{kind: keyQuit}
		}
		return key{kind: keyRune, r: rune(rec.char)}
	}
}

// enableVT turns on ANSI escape handling for the console so the wizard can
// redraw its menus in place. False on consoles that can't do it.
func enableVT() bool {
	h := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	const enableVirtualTerminalProcessing = 0x0004
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
