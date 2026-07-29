package projectkey

import (
	"bytes"
	"crypto/hmac"
	"fmt"
	"strings"
	"testing"
)

// TestTokenRoundTrip proves generated tokens retain their public identifier and 32-byte secret.
func TestTokenRoundTrip(t *testing.T) {
	token, err := Generate(strings.NewReader(strings.Repeat("r", 44)))
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := Parse(token.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PublicID != token.PublicID || len(parsed.Secret) != 32 {
		t.Fatalf("parsed token = %#v", parsed)
	}
}

// TestTokenParseRejectsMalformedValues proves only the canonical fixed-width token format parses.
func TestTokenParseRejectsMalformedValues(t *testing.T) {
	valid := "llmgw_k_cnJycnJycnJycnJy.cnJycnJycnJycnJycnJycnJycnJycnJycnJycnJycnI"

	tests := []struct {
		name string
		raw  string
	}{
		{name: "short total length", raw: valid[:len(valid)-1]},
		{name: "long total length", raw: valid + "x"},
		{name: "wrong prefix", raw: "llmgw_x_" + valid[len(Prefix):]},
		{name: "missing separator", raw: valid[:24] + "_" + valid[25:]},
		{name: "padded base64", raw: valid[:23] + "=" + valid[24:]},
		{name: "public ID wrong decoded size", raw: valid[:16] + "!" + valid[17:]},
		{name: "secret wrong decoded size", raw: valid[:40] + "!" + valid[41:]},
		{name: "non-canonical trailing bits", raw: valid[:len(valid)-1] + "J"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.raw); err == nil {
				t.Fatalf("Parse(%q) succeeded", test.raw)
			}
		})
	}
}

// TestTokenGenerateRequiresAllEntropy proves partial random input cannot create a credential.
func TestTokenGenerateRequiresAllEntropy(t *testing.T) {
	if _, err := Generate(strings.NewReader(strings.Repeat("r", 43))); err == nil {
		t.Fatal("Generate with 43 random bytes succeeded")
	}
}

// TestDigestMatchesHMACSHA256KnownAnswer proves Digest implements keyed HMAC-SHA-256.
func TestDigestMatchesHMACSHA256KnownAnswer(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x0b}, 20)
	const (
		raw  = "Hi There"
		want = "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	)

	first := Digest(pepper, raw)
	second := Digest(pepper, raw)
	if first != second {
		t.Fatalf("Digest is not deterministic: %x != %x", first, second)
	}
	if got := fmt.Sprintf("%x", first); got != want {
		t.Fatalf("Digest = %s, want RFC 4231 HMAC-SHA-256 %s", got, want)
	}

	changedToken := Digest(pepper, raw[:len(raw)-1]+"x")
	if hmac.Equal(first[:], changedToken[:]) {
		t.Fatal("hmac.Equal accepted a one-byte token change")
	}

	changedPepper := append([]byte(nil), pepper...)
	changedPepper[len(changedPepper)-1] ^= 1
	pepperDigest := Digest(changedPepper, raw)
	if hmac.Equal(first[:], pepperDigest[:]) {
		t.Fatal("hmac.Equal accepted a one-byte pepper change")
	}
}
