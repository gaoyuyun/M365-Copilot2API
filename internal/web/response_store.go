package web

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

func responseStorePath() string {
	if path := os.Getenv("M365_RESPONSE_CACHE"); path != "" {
		return path
	}
	if dir := os.Getenv("M365_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "responses.json")
	}
	return filepath.Join(os.TempDir(), "m365-copilot2api-responses.json")
}

func loadResponseMessages(path string) map[string]map[string]*RespNode {
	data := map[string]map[string]*RespNode{}
	b, err := os.ReadFile(path)
	if err != nil {
		return data
	}
	if err := json.Unmarshal(b, &data); err != nil {
		log.Printf("[responses] failed to unmarshal %s: %v", path, err)
		return map[string]map[string]*RespNode{}
	}
	for tenant, bucket := range data {
		for id, node := range bucket {
			if node == nil {
				delete(bucket, id)
			}
		}
		if len(bucket) == 0 {
			delete(data, tenant)
		}
	}
	return data
}

func (s *Server) persistResponsesLocked() {
	if s.responsePath == "" {
		return
	}
	b, err := json.MarshalIndent(s.responseMessages, "", "  ")
	if err != nil {
		log.Printf("[responses] marshal cache: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.responsePath), 0o700); err != nil {
		log.Printf("[responses] create cache directory: %v", err)
		return
	}
	if err := writeFileAtomic(s.responsePath, b, 0o600); err != nil {
		log.Printf("[responses] persist cache: %v", err)
	}
}

func (s *Server) storeResponseNode(namespace, id string, node *RespNode) {
	s.responseMu.Lock()
	defer s.responseMu.Unlock()
	if s.responseMessages == nil {
		s.responseMessages = map[string]map[string]*RespNode{}
	}
	bucket := s.responseMessages[namespace]
	if bucket == nil {
		bucket = map[string]*RespNode{}
		s.responseMessages[namespace] = bucket
	}
	now := time.Now()
	for key, existing := range bucket {
		if existing == nil || now.Sub(existing.At) > time.Hour {
			delete(bucket, key)
		}
	}
	if len(bucket) >= maxResponsesPerTenant {
		var oldestKey string
		var oldestAt time.Time
		for key, existing := range bucket {
			if oldestKey == "" || existing.At.Before(oldestAt) {
				oldestKey, oldestAt = key, existing.At
			}
		}
		delete(bucket, oldestKey)
	}
	bucket[id] = node
	s.persistResponsesLocked()
}
