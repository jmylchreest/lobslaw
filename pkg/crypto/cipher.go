package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/sys/cpu"
)

// Envelope format for values sealed by Cipher:
//
//	magic(4) | alg(1) | nonce(algNonceSize) | ciphertext+tag
//
// Legacy values carry no magic — see OpenTo for how the two are told
// apart. The algorithm is per-record rather than per-store so a cluster
// of mixed hardware can seal with whatever each node does fastest and
// still read every peer's writes.
var envelopeMagic = [4]byte{'L', 'S', 'C', '1'}

// Alg identifies the AEAD a sealed value was produced with.
type Alg byte

const (
	// AlgLegacySecretbox is not a valid Alg byte — it names the
	// unversioned nacl/secretbox format that predates the envelope.
	AlgLegacySecretbox Alg = 0
	// AlgXChaCha20Poly1305 is the default without AES hardware. 24-byte
	// random nonce, so collisions are not a practical concern.
	AlgXChaCha20Poly1305 Alg = 1
	// AlgAESGCM is the default where the CPU accelerates AES. Faster,
	// but its 96-bit nonce carries a birthday bound: stay under ~2^32
	// sealed values per key. Unreachable for a personal deployment,
	// documented so it is a known limit rather than a discovered one.
	AlgAESGCM Alg = 2
)

func (a Alg) String() string {
	switch a {
	case AlgXChaCha20Poly1305:
		return "xchacha20poly1305"
	case AlgAESGCM:
		return "aes-256-gcm"
	case AlgLegacySecretbox:
		return "secretbox"
	default:
		return fmt.Sprintf("unknown(%d)", byte(a))
	}
}

// hasAESHardware reports whether AES is hardware-accelerated here.
// Without it Go's crypto/aes falls back to a constant-time software
// implementation that is slower than ChaCha20 — which is the whole
// reason ChaCha20 exists, and why this is a runtime choice rather than
// a build-time one. Notably absent on some ARM SBCs.
func hasAESHardware() bool {
	return cpu.X86.HasAES || cpu.ARM64.HasAES || cpu.ARM.HasAES ||
		cpu.S390X.HasAES || cpu.PPC64.IsPOWER8
}

// Cipher seals and opens values for one key. Construct it once and
// reuse it: building an AES-GCM AEAD costs an allocation and a key
// expansion, which is wasted per-record work on a scan that decrypts
// thousands.
//
// Safe for concurrent use — cipher.AEAD is, and Cipher adds no state.
type Cipher struct {
	key       Key
	xchacha   cipher.AEAD
	aesgcm    cipher.AEAD
	preferred Alg
}

// NewCipher prepares both AEADs. Both are always available for opening;
// only the default for sealing depends on the CPU.
func NewCipher(key Key) (*Cipher, error) {
	xchacha, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("xchacha20poly1305: %w", err)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aes-gcm: %w", err)
	}
	preferred := AlgXChaCha20Poly1305
	if hasAESHardware() {
		preferred = AlgAESGCM
	}
	return &Cipher{key: key, xchacha: xchacha, aesgcm: aesgcm, preferred: preferred}, nil
}

// Preferred reports the algorithm Seal will use on this machine.
func (c *Cipher) Preferred() Alg { return c.preferred }

func (c *Cipher) aead(a Alg) (cipher.AEAD, error) {
	switch a {
	case AlgXChaCha20Poly1305:
		return c.xchacha, nil
	case AlgAESGCM:
		return c.aesgcm, nil
	default:
		return nil, fmt.Errorf("unknown algorithm %d", byte(a))
	}
}

// Seal encrypts plaintext under the preferred algorithm.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	aead, err := c.aead(c.preferred)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	out := make([]byte, len(envelopeMagic)+1+ns, len(envelopeMagic)+1+ns+len(plaintext)+aead.Overhead())
	copy(out, envelopeMagic[:])
	out[len(envelopeMagic)] = byte(c.preferred)
	nonce := out[len(envelopeMagic)+1:]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(out, nonce, plaintext, nil), nil
}

// OpenTo decrypts sealed, appending the plaintext to dst[:0] when dst
// has the capacity. Pass nil for a fresh buffer.
//
// Reusing dst across a scan is what keeps a full-bucket walk from
// allocating one plaintext per record; callers must therefore not retain
// the returned slice past the next call.
func (c *Cipher) OpenTo(dst []byte, sealed []byte) ([]byte, error) {
	if versioned(sealed) {
		pt, err := c.openVersioned(dst, sealed)
		if err == nil {
			return pt, nil
		}
		// A legacy value whose random nonce happens to begin with the
		// magic lands here (~2^-32 per record). Falling through rather
		// than failing keeps that record readable instead of making the
		// discriminator a source of silent data loss.
	}
	return openLegacy(dst, c.key, sealed)
}

func versioned(sealed []byte) bool {
	return len(sealed) > len(envelopeMagic)+1 &&
		sealed[0] == envelopeMagic[0] && sealed[1] == envelopeMagic[1] &&
		sealed[2] == envelopeMagic[2] && sealed[3] == envelopeMagic[3]
}

func (c *Cipher) openVersioned(dst, sealed []byte) ([]byte, error) {
	body := sealed[len(envelopeMagic):]
	aead, err := c.aead(Alg(body[0]))
	if err != nil {
		return nil, err
	}
	body = body[1:]
	ns := aead.NonceSize()
	if len(body) < ns+aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(dst[:0], body[:ns], body[ns:], nil)
}

// AlgOf reports which algorithm sealed a value, for migration tooling
// and diagnostics. Returns AlgLegacySecretbox for unversioned values.
//
// A legacy value can in principle be misreported here — the same 2^-32
// magic collision OpenTo recovers from. Fine for counting how much of a
// store still needs re-sealing; not a security decision.
func AlgOf(sealed []byte) Alg {
	if !versioned(sealed) {
		return AlgLegacySecretbox
	}
	return Alg(sealed[len(envelopeMagic)])
}
