package bdexperiment

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// SchemaVersion identifies the append-only experiment observation format.
const SchemaVersion = 1

// Record is the privacy-safe, value-free observation for one eligible read.
type Record struct {
	Timestamp        string `json:"ts"`
	Schema           int    `json:"schema"`
	Build            string `json:"build"`
	Arm              Arm    `json:"arm"`
	Verb             string `json:"verb"`
	Shape            Shape  `json:"shape"`
	Disposition      string `json:"disposition"`
	Exit             int    `json:"exit"`
	StdoutBytes      int64  `json:"stdout_bytes"`
	ConfigGeneration string `json:"config_generation"`
	MainMS           int64  `json:"main_ms"`
	DispatcherMS     int64  `json:"dispatcher_ms"`
}

// Append adds one validated observation. It is best-effort: false means that
// logging failed and callers must preserve the command result unchanged.
func Append(path string, record Record) bool {
	if !validRecord(record) {
		return false
	}
	record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(record)
	if err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck // best-effort observation
	_, err = f.Write(append(encoded, '\n'))
	return err == nil
}

func validRecord(record Record) bool {
	if record.Schema != SchemaVersion || record.Build == "" || !knownShape(record.Shape) ||
		(record.Arm != ArmShim && record.Arm != ArmDirect && record.Arm != ArmLegacy) ||
		(record.Verb != "show" && record.Verb != "list" && record.Verb != "query" && record.Verb != "mol") ||
		(record.Disposition != "controller" && record.Disposition != "legacy") ||
		record.StdoutBytes < 0 || record.MainMS < 0 || record.DispatcherMS < 0 {
		return false
	}
	_, err := strconv.ParseUint(record.ConfigGeneration, 10, 32)
	return err == nil
}
