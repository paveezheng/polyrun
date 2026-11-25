package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitGoArgs(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantBuildFlags []string
		wantTargets    []string
		wantAppArgs    []string
		wantUseGoRun   bool
	}{
		{
			name:        "empty defaults to current package",
			args:        nil,
			wantTargets: []string{"."},
		},
		{
			name:        "package only",
			args:        []string{"./cmd/mac-tools"},
			wantTargets: []string{"./cmd/mac-tools"},
		},
		{
			name:        "package with app flags and positional args",
			args:        []string{"./cmd/mac-tools", "--show-system-volumes", "token"},
			wantTargets: []string{"./cmd/mac-tools"},
			wantAppArgs: []string{"--show-system-volumes", "token"},
		},
		{
			name:           "build flags before package and app flags after",
			args:           []string{"-race", "-ldflags", "-s -w", "-tags=dev", "./cmd/mac-tools", "--help"},
			wantBuildFlags: []string{"-race", "-ldflags", "-s -w", "-tags=dev"},
			wantTargets:    []string{"./cmd/mac-tools"},
			wantAppArgs:    []string{"--help"},
		},
		{
			name:        "multiple go source files followed by app args",
			args:        []string{"main.go", "helper.go", "--port", "8080"},
			wantTargets: []string{"main.go", "helper.go"},
			wantAppArgs: []string{"--port", "8080"},
		},
		{
			name:         "versioned package falls back to go run",
			args:         []string{"github.com/paveezheng/mac-tools/cmd/mac-tools@latest"},
			wantUseGoRun: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFlags, gotTargets, gotAppArgs, gotGoRun := splitGoArgs(tc.args)
			if gotGoRun != tc.wantUseGoRun {
				t.Fatalf("useGoRun = %v, want %v", gotGoRun, tc.wantUseGoRun)
			}
			if !reflect.DeepEqual(gotFlags, tc.wantBuildFlags) {
				t.Fatalf("buildFlags = %#v, want %#v", gotFlags, tc.wantBuildFlags)
			}
			if !reflect.DeepEqual(gotTargets, tc.wantTargets) {
				t.Fatalf("targets = %#v, want %#v", gotTargets, tc.wantTargets)
			}
			if !reflect.DeepEqual(gotAppArgs, tc.wantAppArgs) {
				t.Fatalf("appArgs = %#v, want %#v", gotAppArgs, tc.wantAppArgs)
			}
		})
	}
}

func TestTakeSnapshotSkipsHiddenVendorAndNestedModules(t *testing.T) {
	root := t.TempDir()

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("go.mod", "module example.com/root\n\ngo 1.25.0\n")
	mustWrite("cmd/app/main.go", "package main\nfunc main() {}\n")
	mustWrite(".git/hooks/sample.go", "package ignored\n")
	mustWrite(".hidden/secret.go", "package ignored\n")
	mustWrite("bin/output.go", "package ignored\n")
	mustWrite("vendor/pkg/v.go", "package ignored\n")
	mustWrite("nestedmod/go.mod", "module example.com/nested\n")
	mustWrite("nestedmod/main.go", "package main\n")
	mustWrite("customskip/skip.go", "package ignored\n")

	spec, err := getLangSpec("go", []string{"./cmd/app"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := newWatchConfig(root, spec)
	cfg.skipDirs["customskip"] = true

	snap := takeSnapshot(cfg)
	wantKeys := map[string]bool{
		"go.mod":                               true,
		filepath.Join("cmd", "app", "main.go"): true,
	}
	if len(snap) != len(wantKeys) {
		t.Fatalf("snapshot keys = %v, want %v", snap, wantKeys)
	}
	for k := range wantKeys {
		if _, ok := snap[k]; !ok {
			t.Fatalf("missing expected watched file %q in %v", k, snap)
		}
	}
}

func TestWatchChangesDetectsCreateOlderMtimeDeleteAndDebounces(t *testing.T) {
	root := t.TempDir()
	mainFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := getLangSpec("go", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := newWatchConfig(root, spec)
	cfg.pollInterval = 25 * time.Millisecond
	cfg.debounce = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var count atomic.Int32
	triggered := make(chan struct{}, 4)
	go watchChanges(ctx, cfg, func() {
		count.Add(1)
		triggered <- struct{}{}
	})

	// Give watcher time to record initial baseline.
	time.Sleep(40 * time.Millisecond)

	// Rapid burst of writes (simulating save + gofmt) should coalesce into 1 event.
	extraFile := filepath.Join(root, "extra.go")
	if err := os.WriteFile(extraFile, []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(extraFile, []byte("package main\n\nvar x = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for change event after burst writes")
	}
	time.Sleep(80 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Fatalf("expected debounced burst to fire once, got %d", got)
	}

	// Setting an OLDER mtime (e.g. git checkout) must still trigger.
	past := time.Now().Add(-10 * time.Hour)
	if err := os.Chtimes(extraFile, past, past); err != nil {
		t.Fatal(err)
	}
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for older-mtime change event")
	}

	// Deleting a watched file must also trigger.
	if err := os.Remove(extraFile); err != nil {
		t.Fatal(err)
	}
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for file-delete change event")
	}
}

func TestGoBuildPrepareAndRunnerLifecycle(t *testing.T) {
	appDir := t.TempDir()
	goMod := "module example.com/sample\n\ngo 1.25.0\n"
	if err := os.WriteFile(filepath.Join(appDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	goodSrc := `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Println("SELECTED:" + os.Args[1])
}
`
	mainPath := filepath.Join(appDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(goodSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	spec, err := getLangSpec("go", []string{"-C", appDir, ".", "/Volumes/Test"}, tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	cmdName, cmdArgs, buildOut, err := spec.prepare(context.Background())
	if err != nil {
		t.Fatalf("prepare succeeded expectedly, got err=%v output=%s", err, buildOut)
	}

	var stdout bytes.Buffer
	r := newRunner(nil, &stdout)
	if err := r.startCommand(cmdName, cmdArgs); err != nil {
		t.Fatalf("startCommand failed: %v", err)
	}

	select {
	case ev := <-r.exited:
		if ev.err != nil {
			t.Fatalf("expected clean exit 0, got %v", ev.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for built binary to exit")
	}

	if got := strings.TrimSpace(stdout.String()); got != "SELECTED:/Volumes/Test" {
		t.Fatalf("stdout = %q, want %q", got, "SELECTED:/Volumes/Test")
	}

	// Introduce a syntax error and verify prepare surfaces compiler error cleanly.
	if err := os.WriteFile(mainPath, []byte("package main\nfunc main( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, errOut, err := spec.prepare(context.Background())
	if err == nil {
		t.Fatal("expected build failure for broken Go syntax")
	}
	if !strings.Contains(errOut, "main.go") {
		t.Fatalf("expected compiler diagnostics mentioning main.go, got %q", errOut)
	}
}
