package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// idRe is the canonical tunnelID format (spec §3.1): exactly 8 characters
// from [a-z0-9], giving ~41 bits of entropy. Desired IDs are lowercased
// before validation.
var idRe = regexp.MustCompile(`^[a-z0-9]{8}$`)

// TunnelRecord is the on-disk representation of a registered tunnel inside
// data/tunnels.json (spec §6.5). Only ownership data persists; online
// state and request counters never touch disk.
type TunnelRecord struct {
	ID         string    `json:"id"`
	SecretHash string    `json:"secretHash"`
	Target     string    `json:"target"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Tunnel is the in-memory registration for one tunnel ID. It exists for the
// process lifetime once allocated (offline is only a missing session — the
// registry entry is never released, spec §3.1 step 6). All mutable fields
// are atomics so the registry lock never has to be held on the request path.
type Tunnel struct {
	ID         string
	secretHash string // SHA-256 of the ownership secret, base64; set once at allocation
	createdAt  time.Time
	session    atomic.Pointer[Session]
	target     atomic.Value // string
	reqCount   atomic.Uint64
}

// newTunnel creates an unregistered tunnel with an empty target.
func newTunnel(id string) *Tunnel {
	t := &Tunnel{
		ID:        id,
		createdAt: time.Now().UTC(),
	}
	t.target.Store("")
	return t
}

// SetTarget updates the target base URL reported by the tunnel client.
// Safe for concurrent use.
func (t *Tunnel) SetTarget(target string) {
	t.target.Store(target)
}

// Target returns the last target base URL reported for this tunnel, or ""
// if none was reported yet. Safe for concurrent use.
func (t *Tunnel) Target() string {
	v, _ := t.target.Load().(string)
	return v
}

// Session returns the currently online session for this tunnel, or nil if
// the tunnel is offline. Safe for concurrent use.
func (t *Tunnel) Session() *Session {
	return t.session.Load()
}

// setSecretHash records the ownership secret hash. Call only once, when the
// ID is first allocated (not concurrency-guarded by design).
func (t *Tunnel) setSecretHash(hash string) {
	t.secretHash = hash
}

// verifySecret reports whether secret proves ownership of this tunnel: its
// SHA-256 must match the stored hash. A tunnel allocated without a secret
// hash accepts only the empty secret.
func (t *Tunnel) verifySecret(secret string) bool {
	if t.secretHash == "" {
		return secret == ""
	}
	return t.secretHash == hashSecret(secret)
}

// Registry keeps the set of registered tunnels and persists them to a single
// JSON file (spec §6.5: write-on-change, temp file + rename; load on
// startup). All methods are safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
	file    string
}

// NewRegistry creates a registry persisting to <dataDir>/tunnels.json. Call
// Load before serving to restore previously registered tunnels.
func NewRegistry(dataDir string) *Registry {
	return &Registry{
		tunnels: make(map[string]*Tunnel),
		file:    filepath.Join(dataDir, "tunnels.json"),
	}
}

// Load restores tunnels from disk. A missing file is not an error (fresh
// install); malformed records and records with invalid IDs are skipped.
func (r *Registry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := os.ReadFile(r.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var records []TunnelRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for _, rec := range records {
		if !idRe.MatchString(rec.ID) {
			continue
		}
		t := newTunnel(rec.ID)
		t.secretHash = rec.SecretHash
		t.SetTarget(rec.Target)
		t.createdAt = rec.CreatedAt
		r.tunnels[rec.ID] = t
	}
	return nil
}

// saveLocked writes all tunnel records to disk atomically (temp file +
// rename). Caller must hold r.mu.
func (r *Registry) saveLocked() error {
	records := make([]TunnelRecord, 0, len(r.tunnels))
	for _, t := range r.tunnels {
		records = append(records, TunnelRecord{
			ID:         t.ID,
			SecretHash: t.secretHash,
			Target:     t.Target(),
			CreatedAt:  t.createdAt,
		})
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, r.file)
}

// Save persists the current registry state (spec §6.5). Safe for
// concurrent use.
func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

// Register allocates or reuses a tunnel ID (spec §3.1 step 3). The desired
// ID is lowercased and must match ^[a-z0-9]{8}$ to be honoured. Outcomes:
//
//	desired taken, secret matches   → that tunnel, reused=true
//	desired taken, secret mismatch  → random ID,   denied=true (downgrade)
//	desired free or empty           → desired or random ID, fresh secret
//
// A fresh allocation generates a 32-byte secret (returned once, hash kept)
// and persists the registry. The returned error only covers IO/crypto
// failures; ID contention is expressed via reused/denied so the client can
// always connect.
func (r *Registry) Register(desired, secret string) (*Tunnel, bool, bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired = strings.ToLower(strings.TrimSpace(desired))
	if desired != "" && !idRe.MatchString(desired) {
		desired = ""
	}

	var tun *Tunnel
	reused := false
	denied := false

	if desired != "" {
		if existing, ok := r.tunnels[desired]; ok {
			if existing.verifySecret(secret) {
				tun = existing
				reused = true
			} else {
				denied = true
			}
		} else {
			tun = newTunnel(desired)
			r.tunnels[desired] = tun
		}
	}

	if tun == nil {
		for {
			id, err := generateID()
			if err != nil {
				return nil, false, false, "", err
			}
			if _, ok := r.tunnels[id]; !ok {
				tun = newTunnel(id)
				r.tunnels[id] = tun
				break
			}
		}
	}

	newSecret := ""
	if !reused {
		s, err := generateSecret()
		if err != nil {
			return nil, false, false, "", err
		}
		newSecret = s
		tun.setSecretHash(hashSecret(s))
		if err := r.saveLocked(); err != nil {
			return nil, false, false, "", err
		}
	}

	return tun, reused, denied, newSecret, nil
}

// Get returns the tunnel registered under id, or nil if unknown. Safe for
// concurrent use.
func (r *Registry) Get(id string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[id]
}

// SetTarget updates the target of a registered tunnel and persists it.
// It reports whether the tunnel existed.
func (r *Registry) SetTarget(id, target string) bool {
	r.mu.Lock()
	tun, ok := r.tunnels[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	tun.SetTarget(target)
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.saveLocked()
	return true
}

// Count returns the number of registered tunnels (online and offline).
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tunnels)
}

// generateID returns a fresh random 8-character [a-z0-9] tunnel ID.
func generateID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// generateSecret returns a fresh 32-byte ownership secret, base64url
// encoded (delivered once to the client; only its hash is stored).
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret returns the base64 SHA-256 of secret — the only form of the
// ownership secret the server ever stores.
func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return base64.StdEncoding.EncodeToString(h[:])
}
