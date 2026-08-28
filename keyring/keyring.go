// Package keyring wraps the system keychain via go-keyring.
//
// Supported backends:
//   - Linux: Secret Service (D-Bus) preferred, kernel keyring (KeyCtl)
//     fallback when no D-Bus session bus is available.
//   - macOS Keychain / Windows Credential Manager via go-keyring.
//
// Open() returns ErrKeyringUnavailable when no persistent backend can be
// initialized. Callers that tolerate secret loss (e.g. `serve`) may fall
// back to NewMemStore(); callers that must persist (`keyring set`) should
// surface the error to the user instead of silently storing in MemStore.
package keyring

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	gkr "github.com/zalando/go-keyring"
)

// ErrKeyringUnavailable is returned by Open when no persistent keyring
// backend can be initialized.
var ErrKeyringUnavailable = errors.New("keyring: no usable backend available")

// Keyring is the common interface satisfied by Store (persistent system
// backend) and MemStore (in-memory fallback). Factory functions in this
// package return Keyring so callers do not depend on a concrete type.
// Consumers that define their own equivalent interface (e.g.
// plugin/wasmhost.Keyring) accept values of this type via Go's
// structural typing — no conversion needed.
type Keyring interface {
	Get(service, key string) (string, error)
	Set(service, key, value string) error
	Delete(service, key string) error
}

// NewKeyring returns a persistent system keyring, falling back to an
// in-memory store (with a warning) when no backend is available. Use this
// for callers that tolerate secret loss across process restarts (e.g.
// long-running servers). Callers that must persist (e.g. `keyring set`)
// should call Open directly and surface the error.
func NewKeyring() Keyring {
	sysKr, err := Open()
	if err != nil {
		slog.Warn("keyring unavailable, using in-memory fallback (secrets will not persist)",
			"error", err)
		return NewMemStore()
	}
	return sysKr
}

// Store wraps a persistent system backend. Methods accept a service name
// so callers (including WASM plugins) can access keys under different
// namespaces.
type Store struct {
	backend backend
}

// backend is the storage interface satisfied by each concrete backend
// (Secret Service via zalando, Linux kernel keyring, native keychain on
// macOS/Windows).
type backend interface {
	Get(service, key string) (string, error)
	Set(service, key, value string) error
	Delete(service, key string) error
}

// Open returns a persistent Store. On Linux it prefers Secret Service
// (D-Bus), falling back to the user kernel keyring (KeyCtl). On macOS /
// Windows it uses the native keychain. Returns ErrKeyringUnavailable when
// no persistent backend can be initialized.
func Open() (*Store, error) {
	b, err := openBackend()
	if err != nil {
		return nil, err
	}
	return &Store{backend: b}, nil
}

// HasSupport reports whether a persistent keyring backend is available.
func HasSupport() bool {
	_, err := Open()
	return err == nil
}

func (s *Store) Get(service, key string) (string, error) {
	v, err := s.backend.Get(service, key)
	if err != nil {
		if errors.Is(err, gkr.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

func (s *Store) Set(service, key, value string) error {
	if value == "" {
		// Empty value used to silently DELETE the stored secret — combined
		// with Get mapping not-found to ("", nil), a mistaken empty string
		// (e.g. an unset API key) destroyed the credential with no trace.
		// Deletion is explicit: use Delete.
		return fmt.Errorf("keyring: refusing to store an empty value (use Delete to remove)")
	}
	return s.backend.Set(service, key, value)
}

func (s *Store) Delete(service, key string) error {
	err := s.backend.Delete(service, key)
	if errors.Is(err, gkr.ErrNotFound) {
		return nil
	}
	return err
}

// secretServiceBackend wraps zalando/go-keyring (D-Bus on Linux,
// Keychain on macOS, Credential Manager on Windows).
type secretServiceBackend struct{}

func (secretServiceBackend) Get(service, key string) (string, error) {
	return gkr.Get(service, key)
}

func (secretServiceBackend) Set(service, key, value string) error {
	return gkr.Set(service, key, value)
}

func (secretServiceBackend) Delete(service, key string) error {
	return gkr.Delete(service, key)
}

// b64Encode/b64Decode are used by the KeyCtl backend to survive binary
// payloads safely across the kernel keyring API (which expects byte
// slices; we keep parity with hdspace-models/credential encoding too).
func b64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func b64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MemStore is an in-memory keyring fallback for when the system keychain
// is unavailable (no D-Bus on Linux, headless environments, etc.).
// Secrets do NOT persist across process restarts.
type MemStore struct {
	mu   sync.RWMutex
	keys map[string]string // "service/key" → value
}

// NewMemStore creates an in-memory keyring.
func NewMemStore() *MemStore {
	return &MemStore{keys: make(map[string]string)}
}

func (m *MemStore) gk(service, key string) string { return service + "/" + key }

func (m *MemStore) Get(service, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.keys[m.gk(service, key)]
	if !ok {
		// Same contract as the persistent backends (keyctlBackend.Get,
		// secretServiceBackend.Get): a missing key is ErrNotFound. Store.Get
		// maps that to ("", nil) for callers that want the tolerant read;
		// backends must not silently diverge from each other.
		return "", gkr.ErrNotFound
	}
	return v, nil
}

func (m *MemStore) Set(service, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value == "" {
		// Same policy as Store.Set: empty values are refused, deletion is
		// explicit via Delete.
		return fmt.Errorf("keyring: refusing to store an empty value (use Delete to remove)")
	}
	m.keys[m.gk(service, key)] = value
	return nil
}

func (m *MemStore) Delete(service, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, m.gk(service, key))
	return nil
}
