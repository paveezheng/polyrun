package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type langSpec interface {
	ext() string
	isGo() bool
	prepare(ctx context.Context) (cmdName string, cmdArgs []string, buildOutput string, err error)
	cleanupOld(keepPath string)
}

func getLangSpec(lang string, rawArgs []string, tmpDir string) (langSpec, error) {
	switch lang {
	case "go":
		buildFlags, targets, appArgs, useGoRun := splitGoArgs(rawArgs)
		return &goLangSpec{
			tmpDir:     tmpDir,
			rawArgs:    rawArgs,
			buildFlags: buildFlags,
			targets:    targets,
			appArgs:    appArgs,
			useGoRun:   useGoRun,
		}, nil
	case "python":
		return &scriptLangSpec{command: "python", fileExt: ".py", args: rawArgs}, nil
	case "python3":
		return &scriptLangSpec{command: "python3", fileExt: ".py", args: rawArgs}, nil
	default:
		return nil, fmt.Errorf("unsupported language %q (expected go, python, or python3)", lang)
	}
}

type scriptLangSpec struct {
	command string
	fileExt string
	args    []string
}

func (s *scriptLangSpec) ext() string       { return s.fileExt }
func (s *scriptLangSpec) isGo() bool        { return false }
func (s *scriptLangSpec) cleanupOld(string) {}
func (s *scriptLangSpec) prepare(context.Context) (string, []string, string, error) {
	return s.command, s.args, "", nil
}

type goLangSpec struct {
	mu         sync.Mutex
	seq        uint64
	builtBins  []string
	tmpDir     string
	rawArgs    []string
	buildFlags []string
	targets    []string
	appArgs    []string
	useGoRun   bool
}

func (g *goLangSpec) ext() string { return ".go" }
func (g *goLangSpec) isGo() bool  { return true }

func (g *goLangSpec) prepare(ctx context.Context) (string, []string, string, error) {
	if g.useGoRun {
		args := append([]string{"run"}, g.rawArgs...)
		return "go", args, "", nil
	}

	g.mu.Lock()
	g.seq++
	seq := g.seq
	g.mu.Unlock()

	// Write each build to a fresh path so macOS arm64 never hits XNU code-signing
	// cache invalidation from overwriting a previously executed Mach-O inode.
	binPath := filepath.Join(g.tmpDir, fmt.Sprintf("polyrun-bin-%d", seq))
	buildArgs := make([]string, 0, 3+len(g.buildFlags)+len(g.targets))
	buildArgs = append(buildArgs, "build")
	buildArgs = append(buildArgs, g.buildFlags...)
	buildArgs = append(buildArgs, "-o", binPath)
	buildArgs = append(buildArgs, g.targets...)

	cmd := exec.CommandContext(ctx, "go", buildArgs...)
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		_ = os.Remove(binPath)
		return "", nil, out.String(), err
	}

	g.mu.Lock()
	g.builtBins = append(g.builtBins, binPath)
	g.mu.Unlock()

	return binPath, g.appArgs, out.String(), nil
}

func (g *goLangSpec) cleanupOld(keepPath string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	remaining := g.builtBins[:0]
	for _, p := range g.builtBins {
		if p == keepPath {
			remaining = append(remaining, p)
			continue
		}
		_ = os.Remove(p)
	}
	g.builtBins = remaining
}

var goBoolBuildFlags = map[string]bool{
	"a":          true,
	"n":          true,
	"r":          true,
	"v":          true,
	"x":          true,
	"work":       true,
	"race":       true,
	"msan":       true,
	"asan":       true,
	"cover":      true,
	"trimpath":   true,
	"buildvcs":   true,
	"linkshared": true,
	"modcacherw": true,
}

var goValueBuildFlags = map[string]bool{
	"p":             true,
	"asmflags":      true,
	"buildmode":     true,
	"compiler":      true,
	"covermode":     true,
	"coverpkg":      true,
	"gccgoflags":    true,
	"gcflags":       true,
	"installsuffix": true,
	"ldflags":       true,
	"mod":           true,
	"modfile":       true,
	"overlay":       true,
	"pgoprofile":    true,
	"pkgdir":        true,
	"tags":          true,
	"toolexec":      true,
	"C":             true,
}

func splitGoArgs(args []string) (buildFlags, targets, appArgs []string, useGoRun bool) {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		buildFlags = append(buildFlags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			i++
			continue
		}
		if goBoolBuildFlags[name] {
			i++
			continue
		}
		if goValueBuildFlags[name] && i+1 < len(args) {
			i++
			buildFlags = append(buildFlags, args[i])
		}
		i++
	}

	if i >= len(args) {
		return buildFlags, []string{"."}, nil, false
	}

	first := args[i]
	if strings.Contains(first, "@") {
		return nil, nil, nil, true
	}

	if strings.HasSuffix(first, ".go") {
		for i < len(args) && strings.HasSuffix(args[i], ".go") {
			targets = append(targets, args[i])
			i++
		}
	} else {
		targets = []string{first}
		i++
	}

	if i < len(args) {
		appArgs = append([]string(nil), args[i:]...)
	}
	return buildFlags, targets, appArgs, false
}

type exitEvent struct {
	err         error
	started     time.Time
	forceKilled bool
}

type processHandle struct {
	cmd           *exec.Cmd
	ptmx          *os.File
	copyDone      chan struct{}
	done          chan error
	started       time.Time
	stopRequested bool
	forceKilled   bool
}

type runner struct {
	bridge *terminalBridge
	stdout io.Writer
	exited chan exitEvent

	mu      sync.Mutex
	current *processHandle
}

func newRunner(bridge *terminalBridge, stdout io.Writer) *runner {
	if stdout == nil {
		stdout = os.Stdout
	}
	return &runner{
		bridge: bridge,
		stdout: stdout,
		exited: make(chan exitEvent, 1),
	}
}

func (r *runner) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil
}

func (r *runner) startCommand(cmdName string, cmdArgs []string) error {
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Env = os.Environ()

	proc := &processHandle{
		cmd:      cmd,
		copyDone: make(chan struct{}),
		done:     make(chan error, 1),
		started:  time.Now(),
	}

	if r.bridge != nil {
		ptmx, err := r.bridge.StartPTY(cmd)
		if err != nil {
			return err
		}
		proc.ptmx = ptmx
		go func(ptmx *os.File, copyDone chan struct{}) {
			defer close(copyDone)
			_, _ = io.Copy(r.stdout, ptmx)
		}(proc.ptmx, proc.copyDone)
	} else {
		close(proc.copyDone)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = os.Stdin
		cmd.Stdout = r.stdout
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
		// Allow the reader goroutine to drain buffered output/TUI restore bytes
		// before closing the master PTY descriptor.
		select {
		case <-proc.copyDone:
		case <-time.After(150 * time.Millisecond):
		}
		_ = proc.ptmx.Close()
		<-proc.copyDone
	}

	proc.done <- err
	close(proc.done)

	r.mu.Lock()
	stopped := proc.stopRequested
	forceKilled := proc.forceKilled
	if r.current == proc {
		r.current = nil
	}
	r.mu.Unlock()

	if r.bridge != nil {
		diedBySignal := exitedBySignal(err) || forceKilled
		r.bridge.ResetTUIModes(diedBySignal)
	}

	if stopped {
		return
	}

	select {
	case r.exited <- exitEvent{err: err, started: proc.started, forceKilled: forceKilled}:
	default:
	}
}

func (r *runner) stop(timeout time.Duration) bool {
	r.mu.Lock()
	proc := r.current
	r.current = nil
	if proc != nil {
		proc.stopRequested = true
	}
	r.mu.Unlock()

	if proc == nil || proc.cmd.Process == nil {
		return false
	}

	pid := proc.cmd.Process.Pid
	if killErr := syscall.Kill(-pid, syscall.SIGTERM); killErr != nil {
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-proc.done:
		return true
	case <-timer.C:
		r.mu.Lock()
		proc.forceKilled = true
		r.mu.Unlock()
		if killErr := syscall.Kill(-pid, syscall.SIGKILL); killErr != nil {
			_ = proc.cmd.Process.Kill()
		}
		<-proc.done
		if r.bridge != nil {
			r.bridge.ResetTUIModes(true)
		}
		return false
	}
}

func exitStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1, false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Exited() {
		return -1, false
	}
	return status.ExitStatus(), true
}

func exitedBySignal(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

func isCleanExit(err error) bool {
	if err == nil {
		return true
	}
	code, ok := exitStatusCode(err)
	return ok && code == 130
}
