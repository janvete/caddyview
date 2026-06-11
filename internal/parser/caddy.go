package parser

import (
	"encoding/json"
	"strings"
	"time"
)

// LogEntry represents one Caddy JSON access log line.
type LogEntry struct {
	Timestamp float64 `json:"ts"`
	Level     string  `json:"level"`
	Logger    string  `json:"logger"`
	Request   struct {
		Method string `json:"method"`
		URI    string `json:"uri"`
		Host   string `json:"host"`
		Proto  string `json:"proto"`
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
	if entry.Request.Host == "" {
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

// Stats holds aggregated metrics for a time window.
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

// BytesPerSec calculates transfer rate given a window duration in seconds.
func (s *Stats) BytesPerSec(windowSecs float64) float64 {
	if windowSecs <= 0 {
		return 0
	}
	return float64(s.TotalBytes) / windowSecs
}

// Bucket aggregates entries into fixed time buckets for the history view.
type Bucket struct {
	Time     time.Time
	Requests int64
	Bytes    int64
}

// ComputeStats builds a Stats object from a slice of log entries.
func ComputeStats(entries []*LogEntry) *Stats {
	s := NewStats()
	for _, e := range entries {
		s.Add(e)
	}
	return s
}

// Bucketize groups entries into buckets of the given duration.
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
