package rongcloud

import (
	"errors"
	"strings"
	"sync"
)

type StreamAssembler struct {
	streams sync.Map
}

type streamBuffer struct {
	mu       sync.Mutex
	chunks   []string
	nodeID   string
	started  bool
	complete bool
}

func NewStreamAssembler() *StreamAssembler {
	return &StreamAssembler{}
}

func (a *StreamAssembler) StartStream(turnID, nodeID string) {
	buf := &streamBuffer{nodeID: nodeID, started: true}
	a.streams.Store(turnID, buf)
}

func (a *StreamAssembler) AppendChunk(turnID, chunk string) error {
	val, ok := a.streams.Load(turnID)
	if !ok {
		return errors.New("stream not found for turn: " + turnID)
	}
	buf := val.(*streamBuffer)
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if !buf.started {
		return errors.New("stream not started for turn: " + turnID)
	}
	buf.chunks = append(buf.chunks, chunk)
	return nil
}

func (a *StreamAssembler) CompleteStream(turnID string) (string, error) {
	val, ok := a.streams.Load(turnID)
	if !ok {
		return "", errors.New("stream not found for turn: " + turnID)
	}
	buf := val.(*streamBuffer)
	buf.mu.Lock()
	defer buf.mu.Unlock()
	buf.complete = true
	return strings.Join(buf.chunks, ""), nil
}

func (a *StreamAssembler) AbortStream(turnID string) {
	a.streams.Delete(turnID)
}
