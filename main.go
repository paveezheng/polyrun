package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type buildResult struct {
	seq     uint64
	cmdName string
	cmdArgs []string
	output  string
	err     error
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage()
		return 0
	}

	lang := os.Getenv("GORUN_LANG")
	if lang == "" {
		lang = "go"
	}
	watchDir := os.Getenv("GORUN_SCAN_DIR")
	if watchDir == "" {
		watchDir = "."
	}
	restartOnExit := os.Getenv("GORUN_RESTART_ON_EXIT") == "1"

	tmpDir, err := os.MkdirTemp("", "polyrun-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[polyrun] temp dir failed:", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)

	spec, err := getLangSpec(lang, args, tmpDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[polyrun]", err)
		return 2
	}

	stopc := make(chan struct{}, 1)
	rerunc := make(chan struct{}, 1)
	bridge, err := newTerminalBridge(
		func() {
			select {
			case stopc <- struct{}{}:
			default:
			}
		},
		func() {
			select {
			case rerunc <- struct{}{}:
			default:
			}
		},
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[polyrun] terminal setup failed:", err)
		return 1
	}
	defer func() {
		if bridge != nil {
			bridge.Close()
		}
	}()

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()

	fileEvents := make(chan struct{}, 1)
	go watchChanges(watchCtx, newWatchConfig(watchDir, spec), func() {
		select {
		case fileEvents <- struct{}{}:
		default:
		}
	})

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer signal.Stop(sigc)

	r := newRunner(bridge, os.Stdout)
	buildDone := make(chan buildResult, 1)

	var (
		activeBuildSeq    uint64
		cancelActiveBuild context.CancelFunc
		lastCmdName       string
		lastCmdArgs       []string
		dirty             = true
	)

	triggerBuild := func(showTransient bool) {
		if cancelActiveBuild != nil {
			cancelActiveBuild()
		}
		activeBuildSeq++
		seq := activeBuildSeq
		ctx, cancel := context.WithCancel(watchCtx)
		cancelActiveBuild = cancel

		if showTransient && !r.running() {
			if spec.isGo() {
				bridge.TransientStatus("building...")
			} else {
				bridge.TransientStatus("starting...")
			}
		}

		go func() {
			cmdName, cmdArgs, output, buildErr := spec.prepare(ctx)
			if ctx.Err() != nil {
				if cmdName != "" && spec.isGo() {
					_ = os.Remove(cmdName)
				}
				return
			}
			buildDone <- buildResult{
				seq:     seq,
				cmdName: cmdName,
				cmdArgs: cmdArgs,
				output:  output,
				err:     buildErr,
			}
		}()
	}

	defer func() {
		if cancelActiveBuild != nil {
			cancelActiveBuild()
		}
	}()

	triggerBuild(true)

	for {
		select {
		case res := <-buildDone:
			if res.seq != activeBuildSeq {
				if res.cmdName != "" && spec.isGo() {
					_ = os.Remove(res.cmdName)
				}
				continue
			}
			cancelActiveBuild = nil

			if res.err != nil {
				r.stop(1500 * time.Millisecond)
				bridge.StatusLine("build failed:")
				bridge.PrintBlock(res.output)
				bridge.StatusLine("waiting for changes · Enter/r: retry · Ctrl-C/q: quit")
				continue
			}

			dirty = false
			lastCmdName = res.cmdName
			lastCmdArgs = res.cmdArgs

			r.stop(1500 * time.Millisecond)
			spec.cleanupOld(lastCmdName)
			bridge.ClearTransient()
			if res.output != "" {
				bridge.PrintBlock(res.output)
			}
			if startErr := r.startCommand(lastCmdName, lastCmdArgs); startErr != nil {
				bridge.StatusLine(fmt.Sprintf("failed to start process: %v", startErr))
			}

		case <-fileEvents:
			drainSignals(fileEvents)
			dirty = true
			triggerBuild(true)

		case <-rerunc:
			drainSignals(rerunc)
			if r.running() {
				continue
			}
			if dirty || lastCmdName == "" {
				triggerBuild(true)
				continue
			}
			bridge.ClearTransient()
			if startErr := r.startCommand(lastCmdName, lastCmdArgs); startErr != nil {
				dirty = true
				triggerBuild(true)
			}

		case ev := <-r.exited:
			if restartOnExit {
				if ev.err != nil && !isCleanExit(ev.err) {
					bridge.StatusLine(fmt.Sprintf("process failed (%v)", ev.err))
					if time.Since(ev.started) < 500*time.Millisecond {
						bridge.StatusLine("crashed immediately · waiting for changes or Enter/r")
						continue
					}
				}
				if !dirty && lastCmdName != "" {
					if startErr := r.startCommand(lastCmdName, lastCmdArgs); startErr == nil {
						continue
					}
				}
				triggerBuild(true)
				continue
			}

			if code, ok := exitStatusCode(ev.err); ok {
				switch code {
				case 0:
					bridge.StatusLine("exited (0) · Enter/r: rerun · Ctrl-C/q: quit · watching for changes")
				case 130:
					bridge.StatusLine("canceled (130) · Enter/r: rerun · Ctrl-C/q: quit · watching for changes")
				default:
					bridge.StatusLine(fmt.Sprintf("exited (%d) · Enter/r: rerun · Ctrl-C/q: quit · watching for changes", code))
				}
			} else {
				bridge.StatusLine(fmt.Sprintf("process exited (%v) · Enter/r: rerun · Ctrl-C/q: quit · watching for changes", ev.err))
			}

		case <-sigc:
			if cancelActiveBuild != nil {
				cancelActiveBuild()
			}
			r.stop(3 * time.Second)
			return 0

		case <-stopc:
			if cancelActiveBuild != nil {
				cancelActiveBuild()
			}
			r.stop(3 * time.Second)
			return 0
		}
	}
}

func drainSignals(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: polyrun [build-flags] [package-or-script] [args...]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Watches source files and reloads your program when they change.")
	fmt.Fprintln(os.Stderr, "While idle after exit: press Enter/r to rerun, q or Ctrl-C to quit.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Environment variables:")
	fmt.Fprintln(os.Stderr, "  GORUN_LANG             go (default), python, or python3")
	fmt.Fprintln(os.Stderr, "  GORUN_SCAN_DIR         directory to watch (default: .)")
	fmt.Fprintln(os.Stderr, "  GORUN_SKIP_DIRS        extra directories to skip, separated by ':'")
	fmt.Fprintln(os.Stderr, "  GORUN_ALL_FILES        set to 1 to watch all non-hidden files")
	fmt.Fprintln(os.Stderr, "  GORUN_RESTART_ON_EXIT  set to 1 to restart automatically after exit")
}
