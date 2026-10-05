package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type langOption interface {
	cmd() string
	args() []string
	ext() string
}

func getOptions(lang string) langOption {
	switch lang {
	case "python":
		return &pythonOption{}
	case "python3":
		return &python3Option{}
	case "go":
		return &goOption{}
	default:
		panic("unsupported language:" + lang)
	}
}

type pythonOption struct{}
type python3Option struct {
	pythonOption
}

func (o *python3Option) cmd() string {
	return "python3"
}

func (o *pythonOption) cmd() string {
	return "python"
}
func (o *pythonOption) args() []string {
	return []string{}
}
func (o *pythonOption) ext() string {
	return ".py"
}

type goOption struct{}

func (o *goOption) cmd() string {
	return "go"
}
func (o *goOption) args() []string {
	return []string{"run"}
}
func (o *goOption) ext() string {
	return ".go"
}

var startTime = time.Now()

type processHandle struct {
	cmd           *exec.Cmd
	ptmx          *os.File
	done          chan error
	started       time.Time
	stopRequested bool
}

type runner struct {
	opt    langOption
	args   []string
	bridge *terminalBridge
	exited chan struct{}

	mu      sync.Mutex
	current *processHandle
}

type terminalBridge struct {
	mu     sync.RWMutex
	state  *term.State
	ptmx   *os.File
	stop   chan struct{}
	winch  chan os.Signal
	onStop func()
}

func main() {
	events := make(chan struct{}, 1)
	callback := func() {
		select {
		case events <- struct{}{}:
		default:
		}
	}
	lang := os.Getenv("GORUN_LANG")
	if lang == "" {
		lang = "go"
	}
	dir := os.Getenv("GORUN_SCAN_DIR")
	if dir == "" {
		dir = "."
	}
	opt := getOptions(lang)

	stopc := make(chan struct{}, 1)
	bridge, err := newTerminalBridge(func() {
		select {
		case stopc <- struct{}{}:
		default:
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "terminal setup failed:", err)
		os.Exit(1)
	}
	if bridge != nil {
		defer bridge.Close()
	}

	go scanChanges(dir, opt, callback)

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer signal.Stop(sigc)

	r := &runner{
		opt:    opt,
		args:   os.Args[1:],
		bridge: bridge,
		exited: make(chan struct{}, 1),
	}
	if err := r.start(); err != nil {
		fmt.Fprintln(os.Stderr, "Run process failed", err)
		os.Exit(1)
	}

	for {
		select {
		case <-events:
			drainEvents(events)
			fmt.Println("rebuilding...")
			if err := r.restart(); err != nil {
				fmt.Println("Run process failed", err)
			}
		case <-r.exited:
			if err := r.start(); err != nil {
				fmt.Println("Run process failed", err)
			}
		case <-sigc:
			fmt.Println("start gracefully stop")
			r.stop(10*time.Second, true)
			return
		case <-stopc:
			fmt.Println("start gracefully stop")
			r.stop(10*time.Second, true)
			return
		}
	}
}

func newTerminalBridge(onStop func()) (*terminalBridge, error) {
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
		state:  state,
		stop:   make(chan struct{}),
		winch:  make(chan os.Signal, 1),
		onStop: onStop,
	}
	signal.Notify(bridge.winch, syscall.SIGWINCH)
	go bridge.forwardInput()
	go bridge.forwardResize()
	bridge.resizeCurrent()
	return bridge, nil
}

func (b *terminalBridge) Close() {
	if b == nil {
		return
	}
	signal.Stop(b.winch)
	close(b.stop)
	_ = term.Restore(int(os.Stdin.Fd()), b.state)
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

func (b *terminalBridge) forwardInput() {
	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if containsCtrlC(buf[:n]) {
				b.onStop()
				return
			}
			if current := b.current(); current != nil {
				_, _ = current.Write(buf[:n])
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

func containsCtrlC(data []byte) bool {
	for _, ch := range data {
		if ch == 3 {
			return true
		}
	}
	return false
}

func drainEvents(events chan struct{}) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

func (r *runner) start() error {
	args := append(append([]string{}, r.opt.args()...), r.args...)
	cmd := exec.Command(r.opt.cmd(), args...)
	cmd.Env = os.Environ()

	proc := &processHandle{
		cmd:     cmd,
		done:    make(chan error, 1),
		started: time.Now(),
	}

	var err error
	if r.bridge != nil {
		proc.ptmx, err = pty.Start(cmd)
		if err != nil {
			return err
		}
		r.bridge.SetCurrent(proc.ptmx)
		go func(ptmx *os.File) {
			_, _ = io.Copy(os.Stdout, ptmx)
		}(proc.ptmx)
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
	}

	r.mu.Lock()
	r.current = proc
	r.mu.Unlock()

	go r.wait(proc)
	return nil
}

func (r *runner) wait(proc *processHandle) {
	err := proc.cmd.Wait()
	if proc.ptmx != nil {
		r.bridge.ClearCurrent(proc.ptmx)
		_ = proc.ptmx.Close()
	}

	proc.done <- err
	close(proc.done)

	r.mu.Lock()
	stopped := proc.stopRequested
	if r.current == proc {
		r.current = nil
	}
	r.mu.Unlock()

	if stopped {
		return
	}
	if err != nil && !isCleanExit(err) {
		fmt.Println("Run process failed", err)
		if time.Since(proc.started) < 500*time.Millisecond {
			return
		}
	}
	select {
	case r.exited <- struct{}{}:
	default:
	}
}

func (r *runner) restart() error {
	r.stop(1500*time.Millisecond, false)
	return r.start()
}

func (r *runner) stop(timeout time.Duration, verbose bool) {
	r.mu.Lock()
	proc := r.current
	r.current = nil
	if proc != nil {
		proc.stopRequested = true
	}
	r.mu.Unlock()

	if proc == nil || proc.cmd.Process == nil {
		return
	}

	_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGTERM)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-proc.done:
		if verbose {
			fmt.Println("gracefully stop successfully")
		}
	case <-timer.C:
		if verbose {
			fmt.Println("gracefully stop failed, force exit")
		}
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		<-proc.done
	}
}

func isCleanExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.ExitStatus() == 130
}

func scanChanges(watchPath string, opt langOption, callback func()) {
	skipDIRs := map[string]bool{
		".git": true, ".venv": true,
	}
	if s := os.Getenv("GORUN_SKIP_DIRS"); s != "" {
		for _, x := range strings.Split(s, ":") {
			skipDIRs[x] = true
		}
	}
	allFiles := os.Getenv("GORUN_ALL_FILES") == "1"
	for {
		filepath.Walk(watchPath, func(path string, info os.FileInfo, err error) error {
			if skipDIRs[path] {
				return filepath.SkipDir
			}

			// ignore hidden files
			if filepath.Base(path)[0] == '.' {
				return nil
			}

			if (allFiles || filepath.Ext(path) == opt.ext()) && info.ModTime().After(startTime) {
				callback()
				startTime = time.Now()
				return errors.New("done")
			}

			return nil
		})
		time.Sleep(500 * time.Millisecond)
	}
}
