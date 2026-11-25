package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	// Reset mouse reporting, bracketed paste, focus events, Kitty/modifyOtherKeys
	// keyboard enhancements, cursor visibility, and SGR styles without moving the cursor.
	ansiSafeModeReset = "\x1b[<u\x1b[>4m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[?25h\x1b[0m"
	ansiExitAltScreen = "\x1b[?1049l"
)

type terminalBridge struct {
	mu      sync.RWMutex
	state   *term.State
	ptmx    *os.File
	stop    chan struct{}
	winch   chan os.Signal
	onStop  func()
	onRerun func()
}

func newTerminalBridge(onStop, onRerun func()) (*terminalBridge, error) {
	stdinFD := int(os.Stdin.Fd())
	stdoutFD := int(os.Stdout.Fd())
	if !term.IsTerminal(stdinFD) || !term.IsTerminal(stdoutFD) {
		return nil, nil
	}

	state, err := term.MakeRaw(stdinFD)
	if err != nil {
		return nil, err
	}

	bridge := &terminalBridge{
		state:   state,
		stop:    make(chan struct{}),
		winch:   make(chan os.Signal, 1),
		onStop:  onStop,
		onRerun: onRerun,
	}
	signal.Notify(bridge.winch, syscall.SIGWINCH)
	go bridge.forwardInput()
	go bridge.forwardResize()
	return bridge, nil
}

func (b *terminalBridge) Close() {
	if b == nil {
		return
	}
	signal.Stop(b.winch)
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	b.ResetTUIModes(true)
	_ = term.Restore(int(os.Stdin.Fd()), b.state)
}

func (b *terminalBridge) StartPTY(cmd *exec.Cmd) (*os.File, error) {
	var (
		ptmx *os.File
		err  error
	)
	if ws, wsErr := pty.GetsizeFull(os.Stdin); wsErr == nil {
		ptmx, err = pty.StartWithSize(cmd, ws)
	} else {
		ptmx, err = pty.Start(cmd)
	}
	if err != nil {
		return nil, err
	}
	b.SetCurrent(ptmx)
	return ptmx, nil
}

func (b *terminalBridge) SetCurrent(ptmx *os.File) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.ptmx = ptmx
	b.mu.Unlock()
	b.resizeCurrent()
}

func (b *terminalBridge) ClearCurrent(ptmx *os.File) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.ptmx == ptmx {
		b.ptmx = nil
	}
	b.mu.Unlock()
}

func (b *terminalBridge) current() *os.File {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ptmx
}

func (b *terminalBridge) resizeCurrent() {
	if b == nil {
		return
	}
	current := b.current()
	if current == nil {
		return
	}
	_ = pty.InheritSize(os.Stdin, current)
}

func (b *terminalBridge) ResetTUIModes(exitAltScreen bool) {
	if b == nil {
		return
	}
	if exitAltScreen {
		_, _ = os.Stdout.WriteString(ansiExitAltScreen + ansiSafeModeReset)
		return
	}
	_, _ = os.Stdout.WriteString(ansiSafeModeReset)
}

func (b *terminalBridge) TransientStatus(msg string) {
	if b == nil {
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "\r\x1b[K\x1b[2m[polyrun] %s\x1b[0m", msg)
}

func (b *terminalBridge) ClearTransient() {
	if b == nil {
		return
	}
	_, _ = os.Stdout.WriteString("\r\x1b[K")
}

func (b *terminalBridge) StatusLine(msg string) {
	if b == nil {
		fmt.Fprintf(os.Stderr, "[polyrun] %s\n", msg)
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "\r\x1b[K\x1b[2m[polyrun] %s\x1b[0m\r\n", msg)
}

func (b *terminalBridge) PrintBlock(text string) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	if b == nil {
		fmt.Fprintln(os.Stderr, text)
		return
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\n", "\r\n")
	_, _ = fmt.Fprintf(os.Stdout, "\r\x1b[K%s\r\n", normalized)
}

func (b *terminalBridge) forwardInput() {
	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if isCtrlCInput(chunk) {
				if b.onStop != nil {
					b.onStop()
				}
				return
			}
			if current := b.current(); current != nil {
				_, _ = current.Write(chunk)
			} else {
				b.handleIdleInput(chunk)
			}
		}
		if err != nil {
			return
		}
		select {
		case <-b.stop:
			return
		default:
		}
	}
}

func isCtrlCInput(chunk []byte) bool {
	if bytes.IndexByte(chunk, 3) >= 0 {
		return true
	}
	// Support Ctrl-C when child TUI enables Kitty keyboard protocol or xterm modifyOtherKeys.
	return bytes.Contains(chunk, []byte("\x1b[99;5u")) ||
		bytes.Contains(chunk, []byte("\x1b[99;5:1u")) ||
		bytes.Contains(chunk, []byte("\x1b[27;5;99~"))
}

func (b *terminalBridge) handleIdleInput(chunk []byte) {
	// Ignore multi-byte escape replies (OSC/CSI/DCS/mouse) arriving after a TUI exits.
	if len(chunk) != 1 || chunk[0] == 0x1b {
		return
	}
	switch chunk[0] {
	case '\r', '\n', 'r', 'R':
		if b.onRerun != nil {
			b.onRerun()
		}
	case 'q', 'Q', 4:
		if b.onStop != nil {
			b.onStop()
		}
	}
}

func (b *terminalBridge) forwardResize() {
	for {
		select {
		case <-b.stop:
			return
		case <-b.winch:
			b.resizeCurrent()
		}
	}
}
