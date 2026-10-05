# polyrun

Watches source files and restarts the program when they change.

```bash
go install github.com/paveezheng/polyrun@latest
```

```bash
polyrun ./cmd/your-tool
```

By default this runs `go run` with the arguments you pass, and restarts it when a `.go` file under the current directory changes. Ctrl-C stops polyrun.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GORUN_LANG` | `go` | `go`, `python`, or `python3` |
| `GORUN_SCAN_DIR` | `.` | Directory to watch |
| `GORUN_SKIP_DIRS` | `.git`, `.venv` | Extra paths to skip, separated by `:` |
| `GORUN_ALL_FILES` | unset | Set to `1` to watch every non-hidden file |

GPL-3.0. See [LICENSE](LICENSE).
