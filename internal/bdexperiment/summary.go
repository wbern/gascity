package bdexperiment

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// Summary is the strict, offline analysis of one build's observations.
type Summary struct {
	Schema int     `json:"schema"`
	Build  string  `json:"build"`
	Groups []Group `json:"groups"`
}

// Group summarizes one arm and closed command shape.
type Group struct {
	Arm             Arm     `json:"arm"`
	Shape           Shape   `json:"shape"`
	Count           int     `json:"count"`
	SuccessCount    int     `json:"success_count"`
	SuccessRate     float64 `json:"success_rate"`
	P50MainMS       int64   `json:"p50_main_ms"`
	P95MainMS       int64   `json:"p95_main_ms"`
	P50DispatcherMS int64   `json:"p50_dispatcher_ms"`
	P95DispatcherMS int64   `json:"p95_dispatcher_ms"`
}

// Summarize decodes one JSONL artifact strictly. It rejects malformed records,
// mixed schemas, or mixed builds instead of silently combining unlike samples.
func Summarize(input io.Reader) (Summary, error) {
	groups := make(map[string][]Record)
	var summary Summary
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		var record Record
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return Summary{}, fmt.Errorf("decode observation line %d: %w", line, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF || !validRecord(record) {
			return Summary{}, fmt.Errorf("invalid observation line %d", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.Timestamp); err != nil {
			return Summary{}, fmt.Errorf("invalid observation timestamp line %d: %w", line, err)
		}
		if line == 1 {
			summary.Schema, summary.Build = record.Schema, record.Build
		} else if record.Schema != summary.Schema || record.Build != summary.Build {
			return Summary{}, fmt.Errorf("mixed schema or build at observation line %d", line)
		}
		groups[string(record.Arm)+"\x00"+string(record.Shape)] = append(groups[string(record.Arm)+"\x00"+string(record.Shape)], record)
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, fmt.Errorf("read observations: %w", err)
	}
	if line == 0 {
		return Summary{}, fmt.Errorf("empty observation artifact")
	}
	for _, records := range groups {
		group := Group{Arm: records[0].Arm, Shape: records[0].Shape, Count: len(records)}
		main := make([]int64, 0, len(records))
		dispatcher := make([]int64, 0, len(records))
		for _, record := range records {
			if record.Exit == 0 {
				group.SuccessCount++
			}
			main = append(main, record.MainMS)
			dispatcher = append(dispatcher, record.DispatcherMS)
		}
		group.SuccessRate = float64(group.SuccessCount) / float64(group.Count)
		group.P50MainMS, group.P95MainMS = percentile(main, 50), percentile(main, 95)
		group.P50DispatcherMS, group.P95DispatcherMS = percentile(dispatcher, 50), percentile(dispatcher, 95)
		summary.Groups = append(summary.Groups, group)
	}
	sort.Slice(summary.Groups, func(i, j int) bool {
		if summary.Groups[i].Arm != summary.Groups[j].Arm {
			return summary.Groups[i].Arm < summary.Groups[j].Arm
		}
		return summary.Groups[i].Shape < summary.Groups[j].Shape
	})
	return summary, nil
}

func percentile(values []int64, percentile int) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := (len(values) - 1) * percentile / 100
	return values[index]
}
