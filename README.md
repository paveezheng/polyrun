# polyrun

Watches source files and reloads your program when they change.

```bash
go install github.com/paveezheng/polyrun@latest
```

```bash
polyrun ./cmd/your-tool
```

In Go mode, `polyrun` compiles to a temporary binary in the background before swapping the running process (keeping TUIs flicker-free and surfacing compile errors without restart loops). When a child exits on its own, press `Enter` or `r` to rerun immediately, edit files to reload, or press `q` / `Ctrl-C` to quit.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GORUN_LANG` | `go` | `go`, `python`, or `python3` |
| `GORUN_SCAN_DIR` | `.` | Directory to watch |
| `GORUN_SKIP_DIRS` | `.git`, `.venv`, `bin`, `vendor`, `node_modules`, ... | Extra paths or names to skip, separated by `:` |
| `GORUN_ALL_FILES` | unset | Set to `1` to watch every non-hidden file |
| `GORUN_RESTART_ON_EXIT` | unset | Set to `1` to automatically restart after clean exit |

GPL-3.0. See [LICENSE](LICENSE).
