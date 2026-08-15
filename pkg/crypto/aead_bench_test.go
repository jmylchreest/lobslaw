package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/nacl/secretbox"
)

// Throughput of candidate AEADs on a payload the size of one
// VectorRecord (1536-float embedding + ~600 bytes of text). The recall
// scan decrypts one of these per record, so this is the cost that
// dominates a query.
const recordSize = 1536*4 + 600

func payload(b *testing.B) []byte {
	p := make([]byte, recordSize)
	if _, err := io.ReadFull(rand.Reader, p); err != nil {
		b.Fatal(err)
	}
	return p
}

// Current implementation: XSalsa20-Poly1305, fresh output buffer.
func BenchmarkSecretboxOpenAlloc(b *testing.B) {
	var key [32]byte
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		b.Fatal(err)
	}
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		b.Fatal(err)
	}
	sealed := secretbox.Seal(nil, payload(b), &nonce, &key)

	b.SetBytes(recordSize)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, ok := secretbox.Open(nil, sealed, &nonce, &key)
		if !ok {
			b.Fatal("open failed")
		}
		_ = out
	}
}

// Same cipher, reused output buffer.
func BenchmarkSecretboxOpenReuse(b *testing.B) {
	var key [32]byte
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		b.Fatal(err)
	}
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		b.Fatal(err)
	}
	sealed := secretbox.Seal(nil, payload(b), &nonce, &key)
	buf := make([]byte, 0, recordSize)

	b.SetBytes(recordSize)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, ok := secretbox.Open(buf[:0], sealed, &nonce, &key)
		if !ok {
			b.Fatal("open failed")
		}
		_ = out
	}
}

// XChaCha20-Poly1305: same 24-byte random nonce envelope as secretbox,
// so the on-disk layout is unchanged. x/crypto ships AVX2 assembly.
func BenchmarkXChaCha20Poly1305OpenReuse(b *testing.B) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		b.Fatal(err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		b.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		b.Fatal(err)
	}
	sealed := aead.Seal(nil, nonce, payload(b), nil)
	buf := make([]byte, 0, recordSize)

	b.SetBytes(recordSize)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := aead.Open(buf[:0], nonce, sealed, nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// AES-256-GCM: uses AES-NI on amd64/arm64. 12-byte nonce, so the
// envelope would have to change.
func BenchmarkAESGCMOpenReuse(b *testing.B) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		b.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		b.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		b.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		b.Fatal(err)
	}
	sealed := aead.Seal(nil, nonce, payload(b), nil)
	buf := make([]byte, 0, recordSize)

	b.SetBytes(recordSize)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := aead.Open(buf[:0], nonce, sealed, nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// Cost of constructing the AEAD. secretbox derives a subkey per call,
// which is why it cannot be amortised; a cipher.AEAD is built once and
// reused across every record.
func BenchmarkAESGCMConstruct(b *testing.B) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		block, err := aes.NewCipher(key)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := cipher.NewGCM(block); err != nil {
			b.Fatal(err)
		}
	}
}
