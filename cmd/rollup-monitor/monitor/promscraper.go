package monitor

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PromScrapeResult holds parsed Prometheus text-format metrics from a single scrape.
// Metric names include their full label set for disambiguation, e.g.
// "channel_total{stage=\"timed_out\"}" maps to its float64 value.
type PromScrapeResult struct {
	// Flat gauge/counter values keyed by metric name (with labels).
	Metrics map[string]float64
}

// Get returns the value for a metric whose name contains the given suffix,
// ignoring any namespace prefix (e.g. "op_batcher_default_"). This handles
// OP Stack's variable namespace without the caller needing to know the proc name.
// Returns 0 and false if not found.
func (r *PromScrapeResult) Get(suffix string) (float64, bool) {
	for k, v := range r.Metrics {
		if strings.HasSuffix(k, suffix) || strings.Contains(k, suffix) {
			return v, true
		}
	}
	return 0, false
}

// GetLabeled returns the value for a metric matching the suffix and containing
// all specified label key=value pairs. For example:
//
//	GetLabeled("channel_total", "stage", "timed_out")
//
// matches "op_batcher_default_channel_total{stage=\"timed_out\"}".
func (r *PromScrapeResult) GetLabeled(suffix string, labelPairs ...string) (float64, bool) {
	if len(labelPairs)%2 != 0 {
		return 0, false
	}
	for k, v := range r.Metrics {
		if !strings.Contains(k, suffix) {
			continue
		}
		match := true
		for i := 0; i < len(labelPairs); i += 2 {
			needle := fmt.Sprintf(`%s="%s"`, labelPairs[i], labelPairs[i+1])
			if !strings.Contains(k, needle) {
				match = false
				break
			}
		}
		if match {
			return v, true
		}
	}
	return 0, false
}

// BatcherPendingBlocksCount returns the live pending L2 block count from op-batcher's
// pending_blocks_count gauge. The metric is a vec labeled by pipeline stage; stage=added
// is updated each time blocks are processed and reflects the current queue depth.
// Falls back to Get("pending_blocks_count") for exporters without a stage label.
func (r *PromScrapeResult) BatcherPendingBlocksCount() (float64, bool) {
	if r == nil {
		return 0, false
	}
	if v, ok := r.GetLabeled("pending_blocks_count", "stage", "added"); ok {
		return v, true
	}
	return r.Get("pending_blocks_count")
}

// ScrapePrometheus fetches and parses Prometheus text-format metrics from url.
// It skips comment lines and HELP/TYPE metadata, keeping only sample lines.
func ScrapePrometheus(ctx context.Context, url string) (*PromScrapeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}

	result := &PromScrapeResult{
		Metrics: make(map[string]float64, 128),
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, value, ok := parsePromLine(line)
		if !ok {
			continue
		}
		result.Metrics[name] = value
	}

	return result, scanner.Err()
}

// parsePromLine parses a single Prometheus text-format sample line into its
// metric name (including labels) and float64 value.
// Example: `http_requests_total{method="GET",code="200"} 1027 1395066363000`
// Returns ("http_requests_total{method=\"GET\",code=\"200\"}", 1027, true).
func parsePromLine(line string) (string, float64, bool) {
	// Find the boundary between metric name (possibly with labels) and value.
	// Labels are enclosed in {}, so we need to skip past them.
	idx := 0
	if braceStart := strings.IndexByte(line, '{'); braceStart != -1 {
		braceEnd := strings.IndexByte(line[braceStart:], '}')
		if braceEnd == -1 {
			return "", 0, false
		}
		idx = braceStart + braceEnd + 1
	}

	// After the name+labels, find the space before the value.
	rest := line[idx:]
	spaceIdx := strings.IndexByte(rest, ' ')
	if spaceIdx == -1 {
		return "", 0, false
	}

	name := line[:idx+spaceIdx]
	valueStr := rest[spaceIdx+1:]

	// There may be a trailing timestamp; take only the first field.
	if sp := strings.IndexByte(valueStr, ' '); sp != -1 {
		valueStr = valueStr[:sp]
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return "", 0, false
	}

	return name, value, true
}

// ScrapePprofGoroutines fetches the goroutine count from a pprof endpoint.
// It requests /debug/pprof/goroutine?debug=1 and counts the "goroutine" header lines.
func ScrapePprofGoroutines(ctx context.Context, baseURL string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/debug/pprof/goroutine?debug=1"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	count := 0
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "goroutine ") {
			count++
		}
	}
	return count, scanner.Err()
}

// ScrapePprofHeapInuse fetches the current heap-in-use bytes from a pprof endpoint.
// It requests /debug/pprof/heap?debug=1 and parses the HeapInuse line.
func ScrapePprofHeapInuse(ctx context.Context, baseURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/debug/pprof/heap?debug=1"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		// Look for "# HeapInuse = 12345" in the pprof header comments
		if strings.HasPrefix(line, "# HeapInuse") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				val, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
				if err == nil {
					return val, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("HeapInuse not found in pprof heap output")
}
