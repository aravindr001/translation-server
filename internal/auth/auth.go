package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// KeyEntry is one entry in the key file. Only a SHA-256 hash of the raw bearer
// token is stored.
type KeyEntry struct {
	Name       string `json:"name"`
	SHA256Hash string `json:"sha256_hash"`
}

type entry struct {
	name string
	hash [sha256.Size]byte
}

// Store holds the currently active keys and can atomically replace them on
// SIGHUP. It never stores raw key material.
type Store struct {
	mu     sync.RWMutex
	path   string
	keys   []entry
	loaded bool
}

// NewStore creates a store and tries to load path. A missing or malformed file
// is returned as an error, but the store remains usable with no keys (fail
// closed). This lets the gateway start at boot without a worker, network, or
// model, while still serving /healthz.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	err := s.Reload()
	return s, err
}

// Reload reads and validates the key file. On any error the old key set is
// retained, making rotation atomic from the caller's perspective.
func (s *Store) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read key file: %w", err)
	}
	keys, err := parseKeys(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.keys = keys
	s.loaded = true
	s.mu.Unlock()
	return nil
}

// Authenticate hashes the presented token and compares it to every active key
// with a constant-time comparison. It returns the matching key name.
func (s *Store) Authenticate(token string) (string, bool) {
	sum := sha256.Sum256([]byte(token))

	s.mu.RLock()
	defer s.mu.RUnlock()

	var name string
	matched := 0
	for i := range s.keys {
		if subtle.ConstantTimeCompare(sum[:], s.keys[i].hash[:]) == 1 {
			name = s.keys[i].name
			matched = 1
		}
	}
	return name, matched == 1
}

// Names returns the sorted active key names. It is used to prune stale rate
// limiter buckets after rotation.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k.name)
	}
	sort.Strings(out)
	return out
}

// Count returns the number of active keys.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Path returns the configured key file path.
func (s *Store) Path() string { return s.path }

func parseKeys(data []byte) ([]entry, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("key file is empty")
	}

	var raw []KeyEntry
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse key file: %w", err)
		}
	case '{':
		var wrapper struct {
			Keys []KeyEntry `json:"keys"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, fmt.Errorf("parse key file: %w", err)
		}
		raw = wrapper.Keys
	default:
		return nil, errors.New("key file must contain a JSON array or an object with a \"keys\" array")
	}

	if len(raw) == 0 {
		return nil, errors.New("key file contains no keys")
	}

	names := make(map[string]struct{}, len(raw))
	keys := make([]entry, 0, len(raw))
	for i, r := range raw {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			return nil, fmt.Errorf("key entry %d has an empty name", i)
		}
		if _, exists := names[name]; exists {
			return nil, fmt.Errorf("key entry %d has a duplicate name", i)
		}
		names[name] = struct{}{}

		hashHex := strings.TrimSpace(r.SHA256Hash)
		hashBytes, err := hex.DecodeString(hashHex)
		if err != nil || len(hashBytes) != sha256.Size {
			return nil, fmt.Errorf("key entry %q has an invalid sha256_hash (need 64 hex characters)", name)
		}
		var h [sha256.Size]byte
		copy(h[:], hashBytes)
		keys = append(keys, entry{name: name, hash: h})
	}
	return keys, nil
}

// GenerateKey returns a fresh 32-byte high-entropy key and the JSON entry to
// add to the key file. The raw key is returned to the caller only.
func GenerateKey(name string) (rawKey string, entry KeyEntry, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", KeyEntry{}, errors.New("name must not be empty")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", KeyEntry{}, fmt.Errorf("generate random key: %w", err)
	}
	rawKey = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(rawKey))
	return rawKey, KeyEntry{Name: name, SHA256Hash: hex.EncodeToString(sum[:])}, nil
}
