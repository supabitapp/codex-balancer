package main

import (
	"bytes"
	"encoding/json"
)

const maxSSEEvent = 1 << 20

type tokenUsage struct {
	input  int64
	output int64
}

type sniffer struct {
	pending bytes.Buffer
	usage   tokenUsage
}

func (s *sniffer) feed(chunk []byte) {
	if s.pending.Len() > maxSSEEvent {
		s.pending.Reset()
	}
	s.pending.Write(chunk)
	for {
		raw := s.pending.Bytes()
		i := bytes.Index(raw, []byte("\n\n"))
		if i < 0 {
			return
		}
		s.read(raw[:i])
		s.pending.Next(i + 2)
	}
}

func (s *sniffer) read(block []byte) {
	for line := range bytes.Lines(block) {
		data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
		if !ok {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &event) != nil {
			continue
		}
		if u := event.Response.Usage; u.InputTokens > 0 || u.OutputTokens > 0 {
			s.usage = tokenUsage{input: u.InputTokens, output: u.OutputTokens}
		}
	}
}
