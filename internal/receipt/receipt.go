// Package receipt produces signed, verifiable proof that a sandbox was destroyed
package receipt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type Receipt struct {
	Session   string    `json:"session"`
	Backend   string    `json:"backend"`
	Resource  string    `json:"resource"`
	Image     string    `json:"image,omitempty"`
	Host      string    `json:"host"`
	Created   time.Time `json:"created"`
	Destroyed time.Time `json:"destroyed"`
	Verified  bool      `json:"verified"` // true only if re-inspection found nothing
	Signature []byte    `json:"signature,omitempty"`
}

// payload is the exact bytes that get signed: the receipt minus its signature
// encoding/json writes struct fields in declaration order, so this is stable
func (r Receipt) payload() ([]byte, error) {
	r.Signature = nil
	return json.Marshal(r)
}

func Sign(r Receipt, key ed25519.PrivateKey) (Receipt, error) {
	p, err := r.payload()
	if err != nil {
		return r, err
	}
	r.Signature = ed25519.Sign(key, p)
	return r, nil
}

func Verify(r Receipt, pub ed25519.PublicKey) bool {
	p, err := r.payload()
	return err == nil && len(r.Signature) == ed25519.SignatureSize && ed25519.Verify(pub, p, r.Signature)
}

// LoadOrCreateKey keeps a 32-byte seed at path (mode 0600).
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	seed, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: corrupt key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func Save(dir string, r Receipt) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, r.Session+".json")
	return p, os.WriteFile(p, b, 0o600)
}
