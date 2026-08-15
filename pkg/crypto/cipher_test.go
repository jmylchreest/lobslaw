package crypto

import (
	"bytes"
	"testing"

	"golang.org/x/crypto/nacl/secretbox"
)

func testKey(t *testing.T) Key {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCipherRoundTrip(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	for _, pt := range [][]byte{
		nil,
		[]byte(""),
		[]byte("short"),
		bytes.Repeat([]byte("x"), 6_744), // one VectorRecord
	} {
		sealed, err := c.Seal(pt)
		if err != nil {
			t.Fatalf("seal %d bytes: %v", len(pt), err)
		}
		got, err := c.OpenTo(nil, sealed)
		if err != nil {
			t.Fatalf("open %d bytes: %v", len(pt), err)
		}
		if !bytes.Equal(got, pt) && !(len(got) == 0 && len(pt) == 0) {
			t.Errorf("round trip of %d bytes mismatched", len(pt))
		}
	}
}

// Both algorithms must open regardless of which this machine prefers, so
// a store written on one node reads on another.
func TestCipherOpensBothAlgorithms(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("cluster of mixed hardware")

	for _, alg := range []Alg{AlgXChaCha20Poly1305, AlgAESGCM} {
		forced := &Cipher{key: c.key, xchacha: c.xchacha, aesgcm: c.aesgcm, preferred: alg}
		sealed, err := forced.Seal(plaintext)
		if err != nil {
			t.Fatalf("%s seal: %v", alg, err)
		}
		if got := AlgOf(sealed); got != alg {
			t.Errorf("AlgOf = %s, want %s", got, alg)
		}
		got, err := c.OpenTo(nil, sealed)
		if err != nil {
			t.Fatalf("%s open: %v", alg, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("%s round trip mismatched", alg)
		}
	}
}

// Values written before the envelope existed must keep opening, or an
// upgrade silently destroys every memory in the store.
func TestCipherOpensLegacySecretbox(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("sealed by a previous version")

	legacy, err := sealLegacy(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got := AlgOf(legacy); got != AlgLegacySecretbox {
		t.Errorf("AlgOf = %s, want secretbox", got)
	}
	got, err := c.OpenTo(nil, legacy)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("legacy round trip mismatched")
	}
}

// A legacy value whose random nonce happens to start with the envelope
// magic (~2^-32 per record) must still open. Without the fallback in
// OpenTo this is silent, unrecoverable data loss.
func TestCipherOpensLegacyValueThatLooksVersioned(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("unlucky nonce")

	var nonce [NonceSize]byte
	copy(nonce[:], envelopeMagic[:])
	for i := len(envelopeMagic); i < NonceSize; i++ {
		nonce[i] = byte(i)
	}
	sealed := make([]byte, NonceSize, NonceSize+len(plaintext)+secretbox.Overhead)
	copy(sealed, nonce[:])
	sealed = secretbox.Seal(sealed, plaintext, &nonce, (*[KeySize]byte)(&key))

	if !versioned(sealed) {
		t.Fatal("test setup failed to produce a magic-prefixed legacy value")
	}
	got, err := c.OpenTo(nil, sealed)
	if err != nil {
		t.Fatalf("open magic-colliding legacy value: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("round trip mismatched")
	}
}

func TestCipherRejectsTamperedAndTruncated(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("authentic"))
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.OpenTo(nil, tampered); err == nil {
		t.Error("tampered ciphertext opened")
	}

	if _, err := c.OpenTo(nil, sealed[:len(envelopeMagic)+2]); err == nil {
		t.Error("truncated ciphertext opened")
	}

	other, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.OpenTo(nil, sealed); err == nil {
		t.Error("opened under the wrong key")
	}
}

// The buffer handed to OpenTo is reused, so consecutive opens must not
// bleed into each other — this is the invariant Store.ForEach relies on.
func TestCipherOpenToReusesBufferWithoutBleeding(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	long, err := c.Seal(bytes.Repeat([]byte("A"), 4096))
	if err != nil {
		t.Fatal(err)
	}
	short, err := c.Seal([]byte("B"))
	if err != nil {
		t.Fatal(err)
	}

	var buf []byte
	first, err := c.OpenTo(buf, long)
	if err != nil {
		t.Fatal(err)
	}
	buf = first
	if len(first) != 4096 {
		t.Fatalf("first open len = %d, want 4096", len(first))
	}

	second, err := c.OpenTo(buf, short)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "B" {
		t.Errorf("second open = %q, want \"B\" — stale bytes leaked from the reused buffer", second)
	}
}

// No t.Parallel: testing.AllocsPerRun panics during a parallel test.
func TestCipherOpenToAllocatesNothingWhenBufferFits(t *testing.T) {
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal(bytes.Repeat([]byte("x"), 6_744))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 8192)

	allocs := testing.AllocsPerRun(100, func() {
		out, err := c.OpenTo(buf, sealed)
		if err != nil {
			t.Fatal(err)
		}
		_ = out
	})
	if allocs != 0 {
		t.Errorf("OpenTo allocated %.1f times per call with a sufficient buffer; want 0", allocs)
	}
}

func TestPackageLevelSealOpenStillWork(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	plaintext := []byte("via the convenience wrappers")

	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("round trip mismatched")
	}
}

func TestPreferredAlgorithmIsSupported(t *testing.T) {
	t.Parallel()
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.aead(c.Preferred()); err != nil {
		t.Fatalf("preferred algorithm %s is not openable: %v", c.Preferred(), err)
	}
	t.Logf("preferred on this machine: %s (aes hardware: %v)", c.Preferred(), hasAESHardware())
}
