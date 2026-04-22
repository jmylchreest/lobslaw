package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	plaintext := []byte("hello, world. this is a secret.")

	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Error("sealed output contains plaintext — encryption didn't happen")
	}

	got, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	t.Parallel()
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	sealed, _ := Seal(k1, []byte("payload"))
	if _, err := Open(k2, sealed); err == nil {
		t.Error("Open should fail with wrong key")
	}
}

func TestOpenRejectsTampered(t *testing.T) {
	t.Parallel()
	k, _ := GenerateKey()
	sealed, _ := Seal(k, []byte("payload"))
	// Flip one byte in the ciphertext area (after nonce).
	sealed[NonceSize+1] ^= 0xff
	if _, err := Open(k, sealed); err == nil {
		t.Error("Open should fail on tampered ciphertext")
	}
}

func TestOpenRejectsTooShort(t *testing.T) {
	t.Parallel()
	k, _ := GenerateKey()
	if _, err := Open(k, []byte{1, 2, 3}); err == nil {
		t.Error("Open should reject absurdly short input")
	}
}

func TestParseKeyHex(t *testing.T) {
	t.Parallel()
	k1, _ := GenerateKey()
	encoded := hex.EncodeToString(k1[:])
	k2, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey hex: %v", err)
	}
	if k1 != k2 {
		t.Error("parsed hex key differs from original")
	}
}

func TestParseKeyBase64(t *testing.T) {
	t.Parallel()
	k1, _ := GenerateKey()
	encoded := base64.StdEncoding.EncodeToString(k1[:])
	k2, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey base64: %v", err)
	}
	if k1 != k2 {
		t.Error("parsed base64 key differs from original")
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"tooshort",
		strings.Repeat("x", 64), // wrong hex length handled differently, but not valid hex chars
		"!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!", // 43 chars but not valid base64
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseKey(c); err == nil {
				t.Errorf("ParseKey(%q) should have errored", c)
			}
		})
	}
}

// Nonce must be 24 bytes of random — two Seals of the same plaintext
// should produce different ciphertexts.
func TestSealProducesDistinctCiphertexts(t *testing.T) {
	t.Parallel()
	k, _ := GenerateKey()
	plaintext := []byte("same every time")
	a, _ := Seal(k, plaintext)
	b, _ := Seal(k, plaintext)
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext produced identical ciphertexts — nonce not random?")
	}
}
