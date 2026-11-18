package main

import "bytes"

const graphReadMemoEntries = 256
const graphReadMemoBytes = 8 << 20

var graphReadMemoKey = &struct{ name string }{"graphReadMemo"}

type graphReadMemo struct {
	graph     *graph
	responses map[string][]byte
	bytes     int
}

func (m *graphReadMemo) retain(key string, data []byte) {
	if len(m.responses) >= graphReadMemoEntries || len(key)+len(data) > graphReadMemoBytes-m.bytes {
		return
	}
	m.responses[key] = bytes.Clone(data)
	m.bytes += len(key) + len(data)
}
