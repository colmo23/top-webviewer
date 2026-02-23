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
	Time    time.Time
	Procs   []ProcessData // process-level (CPU / Mem tabs)
	Threads []ProcessData // thread-level  (Threads tab)
}

type Series struct {
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
}

type APIResponse struct {
	Timestamps []string `json:"timestamps"`
	CPU        []Series `json:"cpu"`
	Mem        []Series `json:"mem"`
	Thread     []Series `json:"thread"`
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

func (c *Collector) SetTopN(n int) {
	c.mu.Lock()
	c.topN = n
	c.mu.Unlock()
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
	type result struct {
		data []ProcessData
		err  error
	}
	procCh := make(chan result, 1)
	thrCh := make(chan result, 1)

	run := func(ch chan result, args ...string) {
		out, err := exec.Command("top", args...).Output()
		if err != nil {
			ch <- result{err: fmt.Errorf("top %v: %w", args, err)}
			return
		}
		data, err := parseTop(out)
		ch <- result{data: data, err: err}
	}

	go run(procCh, "-b", "-n", "2")
	go run(thrCh, "-b", "-n", "2", "-H")

	pr, tr := <-procCh, <-thrCh
	if pr.err != nil {
		return Snapshot{}, pr.err
	}
	if tr.err != nil {
		return Snapshot{}, tr.err
	}
	return Snapshot{Time: time.Now(), Procs: pr.data, Threads: tr.data}, nil
}

// parseTop parses `top -b -n 2` output and returns all processes from the
// second iteration with both CPU and memory usage, aggregating across processes
// that share a command name. The second iteration has more accurate CPU values.
func parseTop(output []byte) ([]ProcessData, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	inProcs := false
	headerCount := 0

	type agg struct{ cpu, mem float64 }
	aggMap := make(map[string]*agg)
	var order []string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "%CPU") && strings.Contains(line, "COMMAND") {
			headerCount++
			inProcs = (headerCount == 2)
			continue
		}
		if !inProcs {
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

// buildSeries constructs a sorted top-n time series from per-snapshot process
// slices. Series are ordered by descending sum so the busiest entries appear
// first in the legend.
func buildSeries(samples [][]ProcessData, n int, val func(ProcessData) float64) []Series {
	nameSet := make(map[string]bool)
	for _, procs := range samples {
		for _, p := range procs {
			nameSet[p.Name] = true
		}
	}

	data := make(map[string][]float64, len(nameSet))
	for name := range nameSet {
		data[name] = make([]float64, len(samples))
	}
	for i, procs := range samples {
		for _, p := range procs {
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
	procSamples := make([][]ProcessData, len(c.snapshots))
	thrSamples := make([][]ProcessData, len(c.snapshots))
	for i, snap := range c.snapshots {
		timestamps[i] = snap.Time.Format(time.RFC3339)
		procSamples[i] = snap.Procs
		thrSamples[i] = snap.Threads
	}

	resp.Timestamps = timestamps
	resp.CPU = buildSeries(procSamples, c.topN, func(p ProcessData) float64 { return p.CPU })
	resp.Mem = buildSeries(procSamples, c.topN, func(p ProcessData) float64 { return p.Mem })
	resp.Thread = buildSeries(thrSamples, c.topN, func(p ProcessData) float64 { return p.CPU })
	return resp
}

func main() {
	topN := flag.Int("n", 10, "number of top processes to track")
	interval := flag.Duration("interval", 1*time.Second, "collection interval")
	history := flag.Int("history", 240, "number of data points to retain")
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
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			TopN int `json:"topN"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.TopN < 1 || body.TopN > 500 {
			http.Error(w, "topN must be between 1 and 500", http.StatusBadRequest)
			return
		}
		collector.SetTopN(body.TopN)
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("Listening on http://%s", *addr)
	log.Printf("Tracking top %d processes, polling every %v, retaining %d points", *topN, *interval, *history)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
