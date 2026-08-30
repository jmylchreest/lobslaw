package compute

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// SigV4 signs the REQUEST, not the caller: the signature covers the
// method, path, query, a chosen header set and a hash of the body. A
// static key cannot express that, which is why Credential mutates a
// *http.Request rather than returning a string.
//
// ON WHAT THESE TESTS DO AND DO NOT PROVE.
//
// They do not assert AWS's published signature constants. Reproducing
// those from memory risks writing down a wrong expected value and then
// "fixing" the test to match the implementation, which is worse than
// no test — it would look verified while proving nothing.
//
// What they prove instead is that every component the specification
// says is covered actually changes the signature, that the header
// block is canonicalised the way the specification describes, and that
// the body survives being hashed. An implementation that silently
// omitted the body, the query, the region or the date would pass a
// naive round-trip test and fails these.
//
// INTEROP AGAINST A REAL ENDPOINT IS STILL UNPROVEN, and should be
// checked before Bedrock is relied on — see the note in ROADMAP.

func signed(t *testing.T, c *SigV4Credential, method, url, body string) *http.Request {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, url, nil)
	} else {
		r, err = http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	}
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if err := c.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func fixedSigner() *SigV4Credential {
	c := NewSigV4Credential("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"us-east-1", "bedrock")
	c.Now = func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }
	return c
}

func signatureOf(t *testing.T, r *http.Request) string {
	t.Helper()
	auth := r.Header.Get("Authorization")
	_, after, ok := strings.Cut(auth, "Signature=")
	if !ok {
		t.Fatalf("no signature in %q", auth)
	}
	return after
}

// --- structure ---------------------------------------------------------

func TestTheAuthorizationHeaderHasTheRequiredShape(t *testing.T) {
	t.Parallel()
	r := signed(t, fixedSigner(), http.MethodPost, "https://bedrock.us-east-1.amazonaws.com/model/x/invoke", `{"a":1}`)
	auth := r.Header.Get("Authorization")

	for _, want := range []string{
		"AWS4-HMAC-SHA256 ",
		"Credential=AKIDEXAMPLE/20150830/us-east-1/bedrock/aws4_request",
		"SignedHeaders=",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization missing %q:\n%s", want, auth)
		}
	}
	// The date must be the ISO8601 basic form; AWS rejects anything
	// else outright.
	if got := r.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

// Host is signed but lives on the URL rather than in Header. A
// signature over a header the server sees differently does not verify.
func TestHostIsSigned(t *testing.T) {
	t.Parallel()
	r := signed(t, fixedSigner(), http.MethodPost, "https://bedrock.us-east-1.amazonaws.com/x", "{}")
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "host") {
		t.Errorf("host is not in SignedHeaders:\n%s", auth)
	}
	if r.Header.Get("Host") == "" {
		t.Error("Host was not set on the request")
	}
}

// SignedHeaders must be lowercase and sorted, or the server rebuilds a
// different canonical request and the signature fails.
func TestSignedHeadersAreLowercaseAndSorted(t *testing.T) {
	t.Parallel()
	c := fixedSigner()
	r, err := http.NewRequest(http.MethodPost, "https://bedrock.us-east-1.amazonaws.com/x",
		bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Zeta", "1")
	r.Header.Set("Alpha", "2")
	r.Header.Set("Content-Type", "application/json")
	if err := c.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	auth := r.Header.Get("Authorization")
	i := strings.Index(auth, "SignedHeaders=")
	j := strings.Index(auth[i:], ",")
	list := auth[i+len("SignedHeaders=") : i+j]

	names := strings.Split(list, ";")
	for k, n := range names {
		if n != strings.ToLower(n) {
			t.Errorf("%q is not lowercase", n)
		}
		if k > 0 && names[k-1] >= n {
			t.Errorf("not sorted: %v", names)
			break
		}
	}
}

// A temporary credential's session token must be presented, or the
// request is signed correctly and rejected as unauthenticated.
func TestASessionTokenIsSentWhenPresent(t *testing.T) {
	t.Parallel()
	c := fixedSigner()
	c.SessionToken = "FQoGZXIvYXdzEExample"
	r := signed(t, c, http.MethodPost, "https://bedrock.us-east-1.amazonaws.com/x", "{}")

	if got := r.Header.Get("X-Amz-Security-Token"); got != c.SessionToken {
		t.Errorf("X-Amz-Security-Token = %q", got)
	}
	if !strings.Contains(r.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("the session token is sent but not signed")
	}
}

func TestNoSessionTokenHeaderWhenThereIsNone(t *testing.T) {
	t.Parallel()
	r := signed(t, fixedSigner(), http.MethodPost, "https://bedrock.us-east-1.amazonaws.com/x", "{}")
	if got := r.Header.Get("X-Amz-Security-Token"); got != "" {
		t.Errorf("X-Amz-Security-Token = %q for a long-lived key", got)
	}
}

// --- every covered component actually changes the signature ------------

// This is the real test. An implementation that omitted the body, the
// query, the region or the date would still produce a plausible
// signature and pass a naive round trip.
func TestEveryCoveredComponentChangesTheSignature(t *testing.T) {
	t.Parallel()
	const url = "https://bedrock.us-east-1.amazonaws.com/model/x/invoke"
	base := signatureOf(t, signed(t, fixedSigner(), http.MethodPost, url, `{"prompt":"a"}`))

	cases := map[string]func() string{
		"body": func() string {
			return signatureOf(t, signed(t, fixedSigner(), http.MethodPost, url, `{"prompt":"b"}`))
		},
		"method": func() string {
			return signatureOf(t, signed(t, fixedSigner(), http.MethodPut, url, `{"prompt":"a"}`))
		},
		"path": func() string {
			return signatureOf(t, signed(t, fixedSigner(), http.MethodPost,
				"https://bedrock.us-east-1.amazonaws.com/model/y/invoke", `{"prompt":"a"}`))
		},
		"query": func() string {
			return signatureOf(t, signed(t, fixedSigner(), http.MethodPost, url+"?v=2", `{"prompt":"a"}`))
		},
		"host": func() string {
			return signatureOf(t, signed(t, fixedSigner(), http.MethodPost,
				"https://bedrock.eu-west-2.amazonaws.com/model/x/invoke", `{"prompt":"a"}`))
		},
		"region": func() string {
			c := fixedSigner()
			c.Region = "eu-west-2"
			return signatureOf(t, signed(t, c, http.MethodPost, url, `{"prompt":"a"}`))
		},
		"service": func() string {
			c := fixedSigner()
			c.Service = "s3"
			return signatureOf(t, signed(t, c, http.MethodPost, url, `{"prompt":"a"}`))
		},
		"date": func() string {
			c := fixedSigner()
			c.Now = func() time.Time { return time.Date(2015, 8, 31, 12, 36, 0, 0, time.UTC) }
			return signatureOf(t, signed(t, c, http.MethodPost, url, `{"prompt":"a"}`))
		},
		"secret": func() string {
			c := fixedSigner()
			c.SecretAccessKey = "a-different-secret-entirely"
			return signatureOf(t, signed(t, c, http.MethodPost, url, `{"prompt":"a"}`))
		},
		"session token": func() string {
			c := fixedSigner()
			c.SessionToken = "temporary"
			return signatureOf(t, signed(t, c, http.MethodPost, url, `{"prompt":"a"}`))
		},
	}
	for name, vary := range cases {
		if got := vary(); got == base {
			t.Errorf("changing the %s did not change the signature; it is not covered", name)
		}
	}
}

// The same request signed twice at the same instant must produce the
// same signature, or nothing verifies.
func TestSigningIsDeterministic(t *testing.T) {
	t.Parallel()
	const url = "https://bedrock.us-east-1.amazonaws.com/model/x/invoke"
	first := signatureOf(t, signed(t, fixedSigner(), http.MethodPost, url, `{"a":1}`))
	for i := range 10 {
		if got := signatureOf(t, signed(t, fixedSigner(), http.MethodPost, url, `{"a":1}`)); got != first {
			t.Fatalf("run %d differs", i)
		}
	}
}

// --- the body survives -------------------------------------------------

// The sharp bug: a body consumed to hash it and not restored is signed
// correctly and then sent empty. The server computes the hash of
// nothing, the signature does not match, and the error says
// "signature mismatch" rather than "your body vanished".
func TestTheBodySurvivesBeingSigned(t *testing.T) {
	t.Parallel()
	const payload = `{"prompt":"do not lose me"}`
	r := signed(t, fixedSigner(), http.MethodPost,
		"https://bedrock.us-east-1.amazonaws.com/x", payload)

	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("body = %q, want %q", got, payload)
	}
}

// A request with no body at all must still sign — the hash of empty is
// a real value, not an error.
func TestAnEmptyBodySigns(t *testing.T) {
	t.Parallel()
	r := signed(t, fixedSigner(), http.MethodGet, "https://bedrock.us-east-1.amazonaws.com/models", "")
	if r.Header.Get("Authorization") == "" {
		t.Error("a bodyless request produced no signature")
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("no payload hash for an empty body")
	}
}

// --- refusals ----------------------------------------------------------

func TestIncompleteCredentialsAreRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*SigV4Credential{
		"no key":     {SecretAccessKey: "s", Region: "r", Service: "v"},
		"no secret":  {AccessKeyID: "k", Region: "r", Service: "v"},
		"no region":  {AccessKeyID: "k", SecretAccessKey: "s", Service: "v"},
		"no service": {AccessKeyID: "k", SecretAccessKey: "s", Region: "r"},
	} {
		r, err := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Apply(context.Background(), r); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

// The secret must never reach an error message.
func TestARefusalDoesNotCarryTheSecret(t *testing.T) {
	t.Parallel()
	const secret = "wJalrXUtnFEMI-super-secret"
	c := &SigV4Credential{SecretAccessKey: secret}
	r, err := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyErr := c.Apply(context.Background(), r)
	if applyErr == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(applyErr.Error(), secret) {
		t.Errorf("the error carries the secret: %v", applyErr)
	}
}

// It satisfies the interface every driver takes.
var _ Credential = (*SigV4Credential)(nil)
