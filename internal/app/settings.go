package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type fastMode string

const (
	fastModeDefault fastMode = "default"
	fastModeOn      fastMode = "on"
	fastModeOff     fastMode = "off"
)

func (m fastMode) valid() bool {
	return m == fastModeDefault || m == fastModeOn || m == fastModeOff
}

func (m fastMode) label() string {
	switch m {
	case fastModeOn:
		return "Force fast"
	case fastModeOff:
		return "Force standard"
	default:
		return "Default"
	}
}

// Preserve all request fields, including fields unknown to websocketEnvelope.
func (m fastMode) override(data []byte, tier string) ([]byte, string, error) {
	if m != fastModeOn && m != fastModeOff {
		return data, tier, nil
	}
	tier = "default"
	if m == fastModeOn {
		tier = serviceTierPriority
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, "", err
	}
	fields["service_tier"], _ = json.Marshal(tier)
	data, err := json.Marshal(fields)
	return data, tier, err
}

// Closing a policy's notification channel reaches every relay that captured it,
// even a relay that has not yet entered its event loop.
type fastModePolicy struct {
	mu      sync.Mutex
	mode    fastMode
	changed chan struct{}
}

func (p *fastModePolicy) snapshot() (fastMode, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	return p.mode, p.changed
}

func (p *fastModePolicy) init() {
	if p.changed == nil {
		p.mode = fastModeDefault
		p.changed = make(chan struct{})
	}
}

func (p *fastModePolicy) set(mode fastMode) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	if p.mode == mode {
		return false
	}
	p.mode = mode
	close(p.changed)
	p.changed = make(chan struct{})
	return true
}

func (s *server) reloadSettings() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	value, err := s.pool.store.raw.FastMode()
	if err != nil {
		return err
	}
	mode := fastMode(value)
	if !mode.valid() {
		return fmt.Errorf("invalid stored fast mode %q", value)
	}
	if s.fastMode.set(mode) {
		s.log.Info("fast mode applied; restarting existing websockets", "fast_mode", mode)
	}
	return nil
}

func (s *server) watchSettings(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reloadSettings(); err != nil {
				s.log.Warn("settings watch failed", "error", err)
			}
		}
	}
}

func (s *server) saveFastMode(mode fastMode) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if !mode.valid() {
		return fmt.Errorf("invalid fast mode %q", mode)
	}
	if err := s.pool.store.raw.SetFastMode(string(mode)); err != nil {
		return err
	}
	s.fastMode.set(mode)
	return nil
}
