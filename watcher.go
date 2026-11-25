package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type fileStamp struct {
	modTimeNano int64
	size        int64
}

type watchConfig struct {
	root         string
	ext          string
	goMode       bool
	allFiles     bool
	skipDirs     map[string]bool
	pollInterval time.Duration
	debounce     time.Duration
}

var defaultSkipDirs = []string{
	".git",
	".hg",
	".svn",
	".venv",
	"venv",
	"node_modules",
	"vendor",
	"bin",
	"dist",
	"build",
	"target",
	"__pycache__",
	".idea",
	".vscode",
	".cursor",
	".cache",
	".codegraph",
}

func newWatchConfig(watchRoot string, spec langSpec) watchConfig {
	skip := make(map[string]bool, len(defaultSkipDirs))
	for _, d := range defaultSkipDirs {
		skip[d] = true
	}
	if extra := os.Getenv("GORUN_SKIP_DIRS"); extra != "" {
		for _, item := range strings.Split(extra, ":") {
			item = strings.TrimSpace(item)
			if item != "" {
				skip[filepath.Clean(item)] = true
			}
		}
	}

	return watchConfig{
		root:         filepath.Clean(watchRoot),
		ext:          spec.ext(),
		goMode:       spec.isGo(),
		allFiles:     os.Getenv("GORUN_ALL_FILES") == "1",
		skipDirs:     skip,
		pollInterval: 200 * time.Millisecond,
		debounce:     120 * time.Millisecond,
	}
}

func watchChanges(ctx context.Context, cfg watchConfig, onChange func()) {
	prev := takeSnapshot(cfg)
	poll := cfg.pollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	debounce := cfg.debounce
	if debounce <= 0 {
		debounce = 120 * time.Millisecond
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := takeSnapshot(cfg)
			if snapshotsEqual(prev, next) {
				continue
			}
			prev = next

			// Wait for rapid multi-file saves or format-on-save writes to settle.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				timer := time.NewTimer(debounce)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				settled := takeSnapshot(cfg)
				if snapshotsEqual(prev, settled) {
					break
				}
				prev = settled
			}

			onChange()
		}
	}
}

func takeSnapshot(cfg watchConfig) map[string]fileStamp {
	root := cfg.root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = filepath.Clean(resolved)
	}

	hasGoWork := false
	if cfg.goMode && !cfg.allFiles {
		if info, err := os.Stat(filepath.Join(root, "go.work")); err == nil && !info.IsDir() {
			hasGoWork = true
		}
	}

	files := make(map[string]fileStamp)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.Clean(rel)
		name := d.Name()

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if shouldSkipDir(name, rel, path, cfg, hasGoWork) {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		if !shouldWatchFile(name, cfg) {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		files[rel] = fileStamp{
			modTimeNano: info.ModTime().UnixNano(),
			size:        info.Size(),
		}
		return nil
	})

	return files
}

func shouldSkipDir(name, rel, fullPath string, cfg watchConfig, hasGoWork bool) bool {
	if len(name) > 1 && name[0] == '.' {
		return true
	}
	if cfg.skipDirs[name] || cfg.skipDirs[rel] || cfg.skipDirs[fullPath] {
		return true
	}
	// Skip nested independent Go modules unless a go.work workspace ties them together.
	if cfg.goMode && !cfg.allFiles && !hasGoWork {
		if info, err := os.Stat(filepath.Join(fullPath, "go.mod")); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func shouldWatchFile(name string, cfg watchConfig) bool {
	if name == "" || name[0] == '.' {
		return false
	}
	if strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".tmp") {
		return false
	}
	if cfg.allFiles {
		return true
	}
	if cfg.ext != "" && filepath.Ext(name) == cfg.ext {
		return true
	}
	if cfg.goMode {
		switch name {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return true
		}
	}
	return false
}

func snapshotsEqual(a, b map[string]fileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}
