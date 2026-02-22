# top-webview

A Go web server that periodically runs `top` and displays the CPU usage of the top N processes as a live time-series graph in the browser.

![screenshot placeholder](https://via.placeholder.com/800x400?text=top-webview)

## Requirements

- Go 1.21+
- `top` (standard on Linux)

## Usage

```
go run . [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-addr` | `127.0.0.1:5000` | HTTP listen address |
| `-n` | `10` | Number of top processes to track |
| `-interval` | `2s` | How often to sample `top` |
| `-history` | `120` | Number of data points to retain |

Then open the address in your browser.

### Examples

```bash
# defaults: top 10 processes, sampled every 2 s
go run .

# top 5 processes, faster sampling, shorter history
go run . -n 5 -interval 1s -history 60

# listen on all interfaces
go run . -addr 0.0.0.0:5000
```

## How it works

1. A background goroutine runs `top -b -n 1` on the configured interval.
2. CPU usage is parsed and aggregated by process name (so multiple instances of the same command are summed).
3. The top N processes by CPU are stored in a rolling in-memory window.
4. `GET /api/data` returns the full time-series as JSON.
5. The browser polls that endpoint and renders a live Chart.js line graph.

## Build

```bash
go build -o top-webview .
./top-webview -n 5
```
