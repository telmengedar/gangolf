package main

import "encoding/json"

type videoDiagnosticWriter struct {
	records chan map[string]any
}

func (w videoDiagnosticWriter) Write(data []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	w.records <- record
	return len(data), nil
}
