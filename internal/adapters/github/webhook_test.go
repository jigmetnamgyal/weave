package github_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/adapters/github"
)

const testSecret = "a-shared-secret-value-for-tests"

// sign produces the header GitHub would send for a body.
func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestVerifySignatureAcceptsAGenuineDelivery is the control: without it, every
// rejection test below could pass against a function that rejects everything.
func TestVerifySignatureAcceptsAGenuineDelivery(t *testing.T) {
	body := []byte(`{"action":"created","installation":{"id":42}}`)
	if err := github.VerifySignature(testSecret, body, sign(t, testSecret, body)); err != nil {
		t.Fatalf("a correctly signed delivery was rejected: %v", err)
	}
}

func TestVerifySignatureRejects(t *testing.T) {
	body := []byte(`{"action":"created","installation":{"id":42}}`)
	valid := sign(t, testSecret, body)

	tests := map[string]struct {
		secret string
		body   []byte
		header string
		want   error
	}{
		"no signature at all": {
			secret: testSecret, body: body, header: "", want: github.ErrSignatureMissing,
		},
		"whitespace only": {
			secret: testSecret, body: body, header: "   ", want: github.ErrSignatureMissing,
		},
		"missing the algorithm prefix": {
			secret: testSecret, body: body,
			header: strings.TrimPrefix(valid, "sha256="), want: github.ErrSignatureMalformed,
		},
		"not hexadecimal": {
			secret: testSecret, body: body,
			header: "sha256=zzzz", want: github.ErrSignatureMalformed,
		},
		// The SHA-1 header GitHub still sends for old integrations must not be
		// accepted here: honouring it would let a caller choose the weaker
		// hash, which is a downgrade rather than a compatibility nicety.
		"sha1 prefix is not honoured": {
			secret: testSecret, body: body,
			header: "sha1=" + strings.TrimPrefix(valid, "sha256="), want: github.ErrSignatureMalformed,
		},
		"body altered after signing": {
			secret: testSecret, body: append(body, ' '),
			header: valid, want: github.ErrSignatureMismatch,
		},
		"a different secret": {
			secret: "some-other-secret", body: body,
			header: valid, want: github.ErrSignatureMismatch,
		},
		"empty secret does not accept everything": {
			secret: "", body: body, header: valid, want: github.ErrSignatureMismatch,
		},
		// Dropping two hex characters leaves valid hex of the wrong length, so
		// it decodes and then fails to match — mismatch, not malformed.
		"signature two characters short": {
			secret: testSecret, body: body,
			header: valid[:len(valid)-2], want: github.ErrSignatureMismatch,
		},
		// Dropping one leaves an odd number of hex characters, which cannot
		// decode at all.
		"signature an odd number of characters": {
			secret: testSecret, body: body,
			header: valid[:len(valid)-1], want: github.ErrSignatureMalformed,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := github.VerifySignature(tt.secret, tt.body, tt.header)
			if !errors.Is(err, tt.want) {
				t.Errorf("VerifySignature = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestVerifySignatureIsOverExactBytes pins the property the whole endpoint
// rests on.
//
// Two JSON documents that mean the same thing have different bytes, and only
// the bytes GitHub signed can verify. This is why the handler must read the
// raw body and check it before parsing: a handler that decoded first and
// re-encoded to verify would accept whichever re-encoding its JSON library
// happened to produce, which is not what was sent.
func TestVerifySignatureIsOverExactBytes(t *testing.T) {
	signed := []byte(`{"action":"created","installation":{"id":42}}`)
	header := sign(t, testSecret, signed)

	equivalent := [][]byte{
		[]byte(`{"installation":{"id":42},"action":"created"}`),   // reordered keys
		[]byte(`{ "action":"created","installation":{"id":42}}`),  // added whitespace
		[]byte(`{"action":"created","installation":{"id":42.0}}`), // same number, written differently
	}

	for _, body := range equivalent {
		if err := github.VerifySignature(testSecret, body, header); !errors.Is(err, github.ErrSignatureMismatch) {
			t.Errorf("a semantically equivalent but differently encoded body verified: %s", body)
		}
	}
}
