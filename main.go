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
	Name string
	CPU  float64
	Mem  float64
}

type Snapshot struct {
	Time      time.Time
	Processes []ProcessData
}

type Series struct {
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
}

type APIResponse struct {
	Timestamps []string `json:"timestamps"`
	CPU        []Series `json:"cpu"`
	Mem        []Series `json:"mem"`
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
	procs, err := parseTop(out)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parsing top output: %w", err)
	}
	return Snapshot{Time: time.Now(), Processes: procs}, nil
}

// parseTop parses `top -b -n 1` output and returns all processes with both
// CPU and memory usage, aggregating across processes that share a command name.
func parseTop(output []byte) ([]ProcessData, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	inProcs := false

	type agg struct{ cpu, mem float64 }
	aggMap := make(map[string]*agg)
	var order []string

	for scanner.Scan() {
		line := scanner.Text()
		if !inProcs {
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
		mem, err := strconv.ParseFloat(fields[9], 64)
		if err != nil {
			continue
		}
		name := fields[11]
		if _, exists := aggMap[name]; !exists {
			aggMap[name] = &agg{}
			order = append(order, name)
		}
		aggMap[name].cpu += cpu
		aggMap[name].mem += mem
	}

	procs := make([]ProcessData, len(order))
	for i, name := range order {
		procs[i] = ProcessData{Name: name, CPU: aggMap[name].cpu, Mem: aggMap[name].mem}
	}
	return procs, nil
}

// buildSeries constructs a sorted top-n time series from snapshots using the
// provided value extractor. Series are ordered by descending sum across all
// snapshots so the busiest processes appear first in the legend.
func buildSeries(snapshots []Snapshot, n int, val func(ProcessData) float64) []Series {
	nameSet := make(map[string]bool)
	for _, snap := range snapshots {
		for _, p := range snap.Processes {
			nameSet[p.Name] = true
		}
	}

	data := make(map[string][]float64, len(nameSet))
	for name := range nameSet {
		data[name] = make([]float64, len(snapshots))
	}
	for i, snap := range snapshots {
		for _, p := range snap.Processes {
			data[p.Name][i] = val(p)
		}
	}

	type entry struct {
		name string
		vals []float64
		sum  float64
	}
	entries := make([]entry, 0, len(data))
	for name, vals := range data {
		var sum float64
		for _, v := range vals {
			sum += v
		}
		entries = append(entries, entry{name, vals, sum})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].sum > entries[j].sum
	})

	if n > len(entries) {
		n = len(entries)
	}
	series := make([]Series, n)
	for i := range series {
		series[i] = Series{Name: entries[i].name, Values: entries[i].vals}
	}
	return series
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

	timestamps := make([]string, len(c.snapshots))
	for i, snap := range c.snapshots {
		timestamps[i] = snap.Time.Format(time.RFC3339)
	}

	resp.Timestamps = timestamps
	resp.CPU = buildSeries(c.snapshots, c.topN, func(p ProcessData) float64 { return p.CPU })
	resp.Mem = buildSeries(c.snapshots, c.topN, func(p ProcessData) float64 { return p.Mem })
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
