package parser

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogEntry represents one Caddy JSON access log line.
type LogEntry struct {
	Timestamp float64 `json:"ts"`
	Level     string  `json:"level"`
	Logger    string  `json:"logger"`
	Request   struct {
		Method   string `json:"method"`
		URI      string `json:"uri"`
		Host     string `json:"host"`
		Proto    string `json:"proto"`
		RemoteIP string `json:"remote_ip"`
	} `json:"request"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	Status   int     `json:"status"`
}

func (e *LogEntry) Time() time.Time {
	sec := int64(e.Timestamp)
	nsec := int64((e.Timestamp - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// ParseLine parses a single Caddy JSON log line. Returns nil if not a request log.
func ParseLine(line string) *LogEntry {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return nil
	}
	var entry LogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil
	}
	if entry.Request.Host == "" || entry.Status == 0 {
		return nil
	}
	return &entry
}

// -- Stats (used for live view and derived from AggregatedHistory for history) --

type Stats struct {
	TotalRequests int64
	TotalBytes    int64
	StatusCodes   map[int]int64
	TopHosts      map[string]int64
	TopIPs        map[string]int64
	AvgDurationMs float64

	durationSum float64
	durationN   int64
}

func NewStats() *Stats {
	return &Stats{
		StatusCodes: make(map[int]int64),
		TopHosts:    make(map[string]int64),
		TopIPs:      make(map[string]int64),
	}
}

func (s *Stats) Add(e *LogEntry) {
	s.TotalRequests++
	s.TotalBytes += e.Size
	s.StatusCodes[e.Status]++
	s.TopHosts[e.Request.Host]++
	s.TopIPs[e.Request.RemoteIP]++
	s.durationSum += e.Duration * 1000
	s.durationN++
	if s.durationN > 0 {
		s.AvgDurationMs = s.durationSum / float64(s.durationN)
	}
}

func (s *Stats) BytesPerSec(windowSecs float64) float64 {
	if windowSecs <= 0 {
		return 0
	}
	return float64(s.TotalBytes) / windowSecs
}

// -- Bucket (time-series bar chart data) --------------------------------------

type Bucket struct {
	Time     time.Time
	Requests int64
	Bytes    int64
}

// -- AggregatedHistory (server-side aggregation result) -----------------------

// AggregatedBucket is one time bucket returned by server-side awk aggregation.
type AggregatedBucket struct {
	Time                                       time.Time
	Requests, Bytes                            int64
	Status2xx, Status3xx, Status4xx, Status5xx int64
}

// AggregatedHistory holds the merged result of per-file server-side aggregations.
type AggregatedHistory struct {
	Buckets    []AggregatedBucket
	TopHosts   map[string]int64
	TopIPs     map[string]int64
	TotalReqs  int64
	TotalBytes int64
	DurSumSecs float64
}

// FilterSince drops buckets older than the window and recomputes totals from
// the remaining buckets, so the chart and the totals always agree. TopHosts and
// TopIPs come from whole rotated files and may slightly overcount at the oldest
// edge of the window — acceptable for a monitoring view.
func (h *AggregatedHistory) FilterSince(since int64) {
	avgDur := 0.0
	if h.TotalReqs > 0 {
		avgDur = h.DurSumSecs / float64(h.TotalReqs)
	}
	kept := h.Buckets[:0]
	var reqs, bytes int64
	for _, b := range h.Buckets {
		if b.Time.Unix() >= since {
			kept = append(kept, b)
			reqs += b.Requests
			bytes += b.Bytes
		}
	}
	h.Buckets = kept
	h.TotalReqs = reqs
	h.TotalBytes = bytes
	h.DurSumSecs = avgDur * float64(reqs)
}

// ToStats converts aggregated history to a Stats object for use in the TUI panels.
func (h *AggregatedHistory) ToStats() *Stats {
	s := NewStats()
	s.TotalRequests = h.TotalReqs
	s.TotalBytes = h.TotalBytes
	s.TopHosts = h.TopHosts
	s.TopIPs = h.TopIPs
	if h.TotalReqs > 0 {
		s.AvgDurationMs = h.DurSumSecs / float64(h.TotalReqs) * 1000
	}
	for _, b := range h.Buckets {
		s.StatusCodes[200] += b.Status2xx
		s.StatusCodes[301] += b.Status3xx
		s.StatusCodes[404] += b.Status4xx
		s.StatusCodes[500] += b.Status5xx
	}
	return s
}

// ToBuckets re-buckets the hourly server-side buckets into the granularity the
// chart wants (e.g. 6h for the week view) and returns them sorted by time.
func (h *AggregatedHistory) ToBuckets(size time.Duration) []Bucket {
	secs := int64(size.Seconds())
	if secs < 1 {
		secs = 1
	}
	merged := map[int64]*Bucket{}
	for _, ab := range h.Buckets {
		ts := ab.Time.Unix() / secs * secs
		b, ok := merged[ts]
		if !ok {
			b = &Bucket{Time: time.Unix(ts, 0)}
			merged[ts] = b
		}
		b.Requests += ab.Requests
		b.Bytes += ab.Bytes
	}
	out := make([]Bucket, 0, len(merged))
	for _, b := range merged {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// ParseAggregatedOutput parses the concatenated output of per-file server-side
// awk aggregations. Sections may repeat (one set per log file) and are merged:
// bucket counts sum by timestamp, host/IP counts sum by key, totals sum.
//
// Per-file format:
//
//	BUCKETS
//	<unix_ts> <count> <bytes> <2xx> <3xx> <4xx> <5xx>
//	HOSTS
//	<count> <host>
//	IPS
//	<count> <ip>
//	TOTALS
//	<total_reqs> <total_bytes> <duration_sum_secs>
func ParseAggregatedOutput(raw string) (*AggregatedHistory, error) {
	h := &AggregatedHistory{
		TopHosts: make(map[string]int64),
		TopIPs:   make(map[string]int64),
	}
	bucketsByTS := map[int64]*AggregatedBucket{}

	section := ""
	sawSection := false
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "BUCKETS", "HOSTS", "IPS", "TOTALS":
			section = line
			sawSection = true
			continue
		}

		fields := strings.Fields(line)
		switch section {
		case "BUCKETS":
			if len(fields) < 7 {
				continue
			}
			ts, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				continue
			}
			ab, ok := bucketsByTS[ts]
			if !ok {
				ab = &AggregatedBucket{Time: time.Unix(ts, 0)}
				bucketsByTS[ts] = ab
			}
			n := func(i int) int64 { v, _ := strconv.ParseInt(fields[i], 10, 64); return v }
			ab.Requests += n(1)
			ab.Bytes += n(2)
			ab.Status2xx += n(3)
			ab.Status3xx += n(4)
			ab.Status4xx += n(5)
			ab.Status5xx += n(6)

		case "HOSTS":
			if len(fields) < 2 {
				continue
			}
			count, _ := strconv.ParseInt(fields[0], 10, 64)
			host := strings.Join(fields[1:], " ")
			h.TopHosts[host] += count

		case "IPS":
			if len(fields) < 2 {
				continue
			}
			count, _ := strconv.ParseInt(fields[0], 10, 64)
			h.TopIPs[fields[1]] += count

		case "TOTALS":
			if len(fields) < 2 {
				continue
			}
			reqs, _ := strconv.ParseInt(fields[0], 10, 64)
			bytes, _ := strconv.ParseInt(fields[1], 10, 64)
			h.TotalReqs += reqs
			h.TotalBytes += bytes
			if len(fields) >= 3 {
				dur, _ := strconv.ParseFloat(fields[2], 64)
				h.DurSumSecs += dur
			}
		}
	}

	if !sawSection {
		return nil, fmt.Errorf("no aggregation output received from server")
	}

	h.Buckets = make([]AggregatedBucket, 0, len(bucketsByTS))
	for _, ab := range bucketsByTS {
		h.Buckets = append(h.Buckets, *ab)
	}
	sort.Slice(h.Buckets, func(i, j int) bool { return h.Buckets[i].Time.Before(h.Buckets[j].Time) })
	return h, nil
}
