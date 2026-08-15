package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/secretbox"
)

// Sizes fixed by nacl/secretbox. Both are in bytes.
const (
	// KeySize is the length of a secretbox symmetric key.
	KeySize = 32
	// NonceSize is the length of the per-message nonce, which is
	// prepended to every ciphertext this package produces.
	NonceSize = 24
)

// Key is a 32-byte nacl/secretbox symmetric key.
type Key [KeySize]byte

// ParseKey decodes a key from a string. Supported encodings: 64 hex
// characters, or 44-byte standard base64 (32-byte key → 44 b64 chars
// with padding, 43 without). Accepts both with/without padding.
func ParseKey(encoded string) (Key, error) {
	var k Key
	switch len(encoded) {
	case hex.EncodedLen(KeySize):
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return k, fmt.Errorf("hex-decode key: %w", err)
		}
		copy(k[:], decoded)
		return k, nil
	case 44, 43:
		b64 := base64.StdEncoding
		if len(encoded) == 43 {
			b64 = base64.RawStdEncoding
		}
		decoded, err := b64.DecodeString(encoded)
		if err != nil {
			return k, fmt.Errorf("base64-decode key: %w", err)
		}
		if len(decoded) != KeySize {
			return k, fmt.Errorf("key length %d, want %d bytes", len(decoded), KeySize)
		}
		copy(k[:], decoded)
		return k, nil
	default:
		return k, fmt.Errorf("unrecognised key encoding (length %d); want hex-%d or base64-%d", len(encoded), hex.EncodedLen(KeySize), 44)
	}
}

// GenerateKey returns a fresh random key. Used by bootstrap flows.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := io.ReadFull(rand.Reader, k[:]); err != nil {
		return k, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// Seal encrypts plaintext with key, using the preferred AEAD for this
// machine. Convenience wrapper: it builds a Cipher per call, so hot paths
// should hold their own via NewCipher.
func Seal(key Key, plaintext []byte) ([]byte, error) {
	c, err := NewCipher(key)
	if err != nil {
		return nil, err
	}
	return c.Seal(plaintext)
}

// Open decrypts sealed bytes, in either the current envelope format or
// the legacy nacl/secretbox one. Same per-call construction caveat as
// Seal.
func Open(key Key, sealed []byte) ([]byte, error) {
	c, err := NewCipher(key)
	if err != nil {
		return nil, err
	}
	return c.OpenTo(nil, sealed)
}

// sealLegacy produces the pre-envelope format: [24-byte nonce | ciphertext].
// Kept so tests can write legacy values and prove they still open.
func sealLegacy(key Key, plaintext []byte) ([]byte, error) {
	var nonce [NonceSize]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := make([]byte, NonceSize, NonceSize+len(plaintext)+secretbox.Overhead)
	copy(out, nonce[:])
	out = secretbox.Seal(out, plaintext, &nonce, (*[KeySize]byte)(&key))
	return out, nil
}

func openLegacy(dst []byte, key Key, sealed []byte) ([]byte, error) {
	if len(sealed) < NonceSize+secretbox.Overhead {
		return nil, errors.New("ciphertext too short")
	}
	var nonce [NonceSize]byte
	copy(nonce[:], sealed[:NonceSize])
	out, ok := secretbox.Open(dst[:0], sealed[NonceSize:], &nonce, (*[KeySize]byte)(&key))
	if !ok {
		return nil, errors.New("decrypt failed (bad key, nonce, or ciphertext)")
	}
	return out, nil
}
