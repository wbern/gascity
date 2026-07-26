// Command bdexperiment-report summarizes one gc bd experiment JSONL artifact.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/bdexperiment"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: bdexperiment-report <observation.jsonl>")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open observations: %v\n", err)
		os.Exit(1)
	}
	summary, err := bdexperiment.Summarize(f)
	closeErr := f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "summarize observations: %v\n", err)
		os.Exit(1)
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "close observations: %v\n", closeErr)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "write summary: %v\n", err)
		os.Exit(1)
	}
}
