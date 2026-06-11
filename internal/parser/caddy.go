package parser

import (
	"encoding/json"
	"fmt"
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

// ParseLines parses multiple lines and returns valid entries.
func ParseLines(lines []string) []*LogEntry {
	entries := make([]*LogEntry, 0, len(lines))
	for _, l := range lines {
		if e := ParseLine(l); e != nil {
			entries = append(entries, e)
		}
	}
	return entries
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

func ComputeStats(entries []*LogEntry) *Stats {
	s := NewStats()
	for _, e := range entries {
		s.Add(e)
	}
	return s
}

// -- Bucket (time-series bar chart data) --------------------------------------

type Bucket struct {
	Time     time.Time
	Requests int64
	Bytes    int64
}

func Bucketize(entries []*LogEntry, bucketSize time.Duration) []Bucket {
	if len(entries) == 0 {
		return nil
	}
	buckets := map[int64]*Bucket{}
	for _, e := range entries {
		t := e.Time().Truncate(bucketSize)
		key := t.Unix()
		if _, ok := buckets[key]; !ok {
			buckets[key] = &Bucket{Time: t}
		}
		buckets[key].Requests++
		buckets[key].Bytes += e.Size
	}
	result := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		result = append(result, *b)
	}
	return result
}

// -- AggregatedHistory (server-side aggregation result) -----------------------

// AggregatedBucket is one time bucket returned by server-side awk aggregation.
type AggregatedBucket struct {
	Time                          time.Time
	Requests, Bytes               int64
	Status2xx, Status3xx, Status4xx, Status5xx int64
}

// AggregatedHistory holds the full result of a server-side history aggregation.
type AggregatedHistory struct {
	Buckets    []AggregatedBucket
	TopHosts   map[string]int64
	TopIPs     map[string]int64
	TotalReqs  int64
	TotalBytes int64
}

// ToStats converts aggregated history to a Stats object for use in the TUI panels.
func (h *AggregatedHistory) ToStats() *Stats {
	s := NewStats()
	s.TotalRequests = h.TotalReqs
	s.TotalBytes = h.TotalBytes
	s.TopHosts = h.TopHosts
	s.TopIPs = h.TopIPs
	for _, b := range h.Buckets {
		s.StatusCodes[200] += b.Status2xx
		s.StatusCodes[301] += b.Status3xx
		s.StatusCodes[404] += b.Status4xx
		s.StatusCodes[500] += b.Status5xx
	}
	return s
}

// ToBuckets converts aggregated buckets to the Bucket slice used by the bar chart.
func (h *AggregatedHistory) ToBuckets() []Bucket {
	out := make([]Bucket, len(h.Buckets))
	for i, ab := range h.Buckets {
		out[i] = Bucket{Time: ab.Time, Requests: ab.Requests, Bytes: ab.Bytes}
	}
	return out
}

// ParseAggregatedOutput parses the output of the server-side awk aggregation script.
//
// Format:
//
//	BUCKETS
//	<unix_ts> <count> <bytes> <2xx> <3xx> <4xx> <5xx>
//	HOSTS
//	<count> <host>
//	IPS
//	<count> <ip>
//	TOTALS
//	<total_reqs> <total_bytes>
func ParseAggregatedOutput(raw string) (*AggregatedHistory, error) {
	h := &AggregatedHistory{
		TopHosts: make(map[string]int64),
		TopIPs:   make(map[string]int64),
	}

	section := ""
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "BUCKETS", "HOSTS", "IPS", "TOTALS":
			section = line
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
			ab := AggregatedBucket{Time: time.Unix(ts, 0)}
			ab.Requests, _ = strconv.ParseInt(fields[1], 10, 64)
			ab.Bytes, _ = strconv.ParseInt(fields[2], 10, 64)
			ab.Status2xx, _ = strconv.ParseInt(fields[3], 10, 64)
			ab.Status3xx, _ = strconv.ParseInt(fields[4], 10, 64)
			ab.Status4xx, _ = strconv.ParseInt(fields[5], 10, 64)
			ab.Status5xx, _ = strconv.ParseInt(fields[6], 10, 64)
			h.Buckets = append(h.Buckets, ab)

		case "HOSTS":
			if len(fields) < 2 {
				continue
			}
			count, _ := strconv.ParseInt(fields[0], 10, 64)
			host := strings.Join(fields[1:], " ")
			h.TopHosts[host] = count

		case "IPS":
			if len(fields) < 2 {
				continue
			}
			count, _ := strconv.ParseInt(fields[0], 10, 64)
			h.TopIPs[fields[1]] = count

		case "TOTALS":
			if len(fields) < 2 {
				continue
			}
			h.TotalReqs, _ = strconv.ParseInt(fields[0], 10, 64)
			h.TotalBytes, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}

	if section == "" {
		return nil, fmt.Errorf("no aggregation output received from server")
	}
	return h, nil
}
