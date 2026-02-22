package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFiles embed.FS

type ProcessData struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"`
}

type Snapshot struct {
	Time      time.Time
	Processes []ProcessData
}

type Series struct {
	Name string    `json:"name"`
	CPU  []float64 `json:"cpu"`
}

type APIResponse struct {
	Timestamps []string `json:"timestamps"`
	Series     []Series `json:"series"`
	TopN       int      `json:"topN"`
	Interval   float64  `json:"interval"`
}

type Collector struct {
	mu        sync.RWMutex
	snapshots []Snapshot
	maxLen    int
	topN      int
	interval  time.Duration
}

func NewCollector(topN, maxLen int, interval time.Duration) *Collector {
	return &Collector{
		snapshots: make([]Snapshot, 0, maxLen),
		maxLen:    maxLen,
		topN:      topN,
		interval:  interval,
	}
}

func (c *Collector) Start() {
	// Collect once immediately so the UI has data right away.
	if snap, err := c.collect(); err == nil {
		c.mu.Lock()
		c.snapshots = append(c.snapshots, snap)
		c.mu.Unlock()
	}

	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for range ticker.C {
			snap, err := c.collect()
			if err != nil {
				log.Printf("collection error: %v", err)
				continue
			}
			c.mu.Lock()
			c.snapshots = append(c.snapshots, snap)
			if len(c.snapshots) > c.maxLen {
				c.snapshots = c.snapshots[1:]
			}
			c.mu.Unlock()
		}
	}()
}

func (c *Collector) collect() (Snapshot, error) {
	out, err := exec.Command("top", "-b", "-n", "1").Output()
	if err != nil {
		return Snapshot{}, fmt.Errorf("running top: %w", err)
	}
	procs, err := parseTop(out, c.topN)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parsing top output: %w", err)
	}
	return Snapshot{Time: time.Now(), Processes: procs}, nil
}

// parseTop parses `top -b -n 1` output and returns the top n processes by CPU,
// aggregating across processes that share the same command name.
func parseTop(output []byte, n int) ([]ProcessData, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	inProcs := false
	agg := make(map[string]float64)
	var order []string // tracks first-seen order to deduplicate

	for scanner.Scan() {
		line := scanner.Text()
		if !inProcs {
			// The process table header contains both %CPU and COMMAND.
			if strings.Contains(line, "%CPU") && strings.Contains(line, "COMMAND") {
				inProcs = true
			}
			continue
		}

		// Standard top -b columns (0-indexed):
		// 0:PID 1:USER 2:PR 3:NI 4:VIRT 5:RES 6:SHR 7:S 8:%CPU 9:%MEM 10:TIME+ 11:COMMAND
		fields := strings.Fields(line)
		if len(fields) < 12 {
			continue
		}
		cpu, err := strconv.ParseFloat(fields[8], 64)
		if err != nil {
			continue
		}
		name := fields[11]
		if _, exists := agg[name]; !exists {
			order = append(order, name)
		}
		agg[name] += cpu
	}

	// Sort names by descending aggregated CPU.
	sort.Slice(order, func(i, j int) bool {
		return agg[order[i]] > agg[order[j]]
	})

	if n > len(order) {
		n = len(order)
	}
	procs := make([]ProcessData, n)
	for i := range procs {
		procs[i] = ProcessData{Name: order[i], CPU: agg[order[i]]}
	}
	return procs, nil
}

func (c *Collector) GetAPIResponse() APIResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resp := APIResponse{
		TopN:     c.topN,
		Interval: c.interval.Seconds(),
	}
	if len(c.snapshots) == 0 {
		return resp
	}

	// Determine the union of all process names seen across snapshots.
	nameSet := make(map[string]bool)
	for _, snap := range c.snapshots {
		for _, p := range snap.Processes {
			nameSet[p.Name] = true
		}
	}

	timestamps := make([]string, len(c.snapshots))
	seriesData := make(map[string][]float64, len(nameSet))
	for name := range nameSet {
		seriesData[name] = make([]float64, len(c.snapshots))
	}

	for i, snap := range c.snapshots {
		timestamps[i] = snap.Time.Format(time.RFC3339)
		for _, p := range snap.Processes {
			seriesData[p.Name][i] = p.CPU
		}
	}

	// Sort series by total CPU across all snapshots (descending) so the
	// most active processes appear first in the legend.
	type entry struct {
		name string
		data []float64
		sum  float64
	}
	entries := make([]entry, 0, len(seriesData))
	for name, data := range seriesData {
		var sum float64
		for _, v := range data {
			sum += v
		}
		entries = append(entries, entry{name, data, sum})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].sum > entries[j].sum
	})

	series := make([]Series, len(entries))
	for i, e := range entries {
		series[i] = Series{Name: e.name, CPU: e.data}
	}

	resp.Timestamps = timestamps
	resp.Series = series
	return resp
}

func main() {
	topN := flag.Int("n", 10, "number of top processes to track")
	interval := flag.Duration("interval", 2*time.Second, "collection interval")
	history := flag.Int("history", 120, "number of data points to retain")
	addr := flag.String("addr", "127.0.0.1:5000", "HTTP listen address")
	flag.Parse()

	collector := NewCollector(*topN, *history, *interval)
	collector.Start()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(collector.GetAPIResponse())
	})

	log.Printf("Listening on http://%s", *addr)
	log.Printf("Tracking top %d processes, polling every %v, retaining %d points", *topN, *interval, *history)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
