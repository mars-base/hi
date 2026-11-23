package proxy

import (
	"sync"
	"time"

	"github.com/mars-base/hi/pkg/logx"
)

const (
	defaultThoughtSigTTL = 24 * time.Hour
	defaultThoughtSigMax = 16384
)

type thoughtSigEntry struct {
	sig     string
	expires time.Time
}

// thoughtSigStore remembers Gemini thought signatures by tool_use id.
//
// Gemini requires at least one thoughtSignature on the functionCall parts of
// every historical tool turn. Anthropic tool_use blocks carry no such field,
// so the proxy caches the signature it saw in the upstream response and
// replays it on the next request. Entries are in-process only: after a proxy
// restart, affected tool turns are degraded to plain text by the converter.
type thoughtSigStore struct {
	mu  sync.Mutex
	m   map[string]thoughtSigEntry
	ttl time.Duration
	max int
}

func newThoughtSigStore() *thoughtSigStore {
	return &thoughtSigStore{
		m:   make(map[string]thoughtSigEntry),
		ttl: defaultThoughtSigTTL,
		max: defaultThoughtSigMax,
	}
}

// putAll stores sig under every tool_use id of one assistant turn.
func (s *thoughtSigStore) putAll(ids []string, sig string) {
	if s == nil || sig == "" || len(ids) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m)+len(ids) > s.max {
		s.pruneLocked(now)
		for len(s.m)+len(ids) > s.max {
			for k := range s.m {
				delete(s.m, k)
				break
			}
		}
	}
	expires := now.Add(s.ttl)
	for _, id := range ids {
		if id != "" {
			s.m[id] = thoughtSigEntry{sig: sig, expires: expires}
		}
	}
	logx.Debug("gemini sig: stored for %d tool call(s)", len(ids))
}

// getAny returns the first unexpired signature among ids.
func (s *thoughtSigStore) getAny(ids []string) (string, bool) {
	if s == nil {
		return "", false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		entry, ok := s.m[id]
		if ok && now.Before(entry.expires) {
			return entry.sig, true
		}
	}
	return "", false
}

func (s *thoughtSigStore) pruneLocked(now time.Time) {
	for k, entry := range s.m {
		if !now.Before(entry.expires) {
			delete(s.m, k)
		}
	}
}
