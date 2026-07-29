package projectkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const (
	// Prefix identifies an LLMGW project key.
	Prefix = "llmgw_k_"

	publicIDBytes  = 12
	secretBytes    = 32
	encodedIDBytes = 16
	encodedSecret  = 43
	tokenBytes     = len(Prefix) + encodedIDBytes + 1 + encodedSecret
)

var errInvalidToken = errors.New("invalid project key")

// Token is a generated or parsed project credential.
type Token struct {
	Plaintext string // Plaintext is the complete bearer credential.
	PublicID  string // PublicID is the non-secret database lookup identifier.
	Secret    []byte // Secret is the credential's 32-byte random secret.
}

// Generate creates a project credential from exactly 44 bytes read from random.
func Generate(random io.Reader) (Token, error) {
	var publicID [publicIDBytes]byte
	if _, err := io.ReadFull(random, publicID[:]); err != nil {
		return Token{}, fmt.Errorf("read project key public identifier:\n%w", err)
	}

	var secret [secretBytes]byte
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return Token{}, fmt.Errorf("read project key secret:\n%w", err)
	}

	encodedID := base64.RawURLEncoding.EncodeToString(publicID[:])
	encodedSecret := base64.RawURLEncoding.EncodeToString(secret[:])

	return Token{
		Plaintext: Prefix + encodedID + "." + encodedSecret,
		PublicID:  encodedID,
		Secret:    append([]byte(nil), secret[:]...),
	}, nil
}

// Parse validates and decodes one canonical project credential.
func Parse(raw string) (Token, error) {
	if len(raw) != tokenBytes {
		return Token{}, errInvalidToken
	}
	if raw[:len(Prefix)] != Prefix || raw[len(Prefix)+encodedIDBytes] != '.' {
		return Token{}, errInvalidToken
	}

	publicID := raw[len(Prefix) : len(Prefix)+encodedIDBytes]
	secretText := raw[len(Prefix)+encodedIDBytes+1:]
	encoding := base64.RawURLEncoding.Strict()

	var publicIDValue [publicIDBytes]byte
	decodedIDBytes, err := encoding.Decode(publicIDValue[:], []byte(publicID))
	if err != nil || decodedIDBytes != publicIDBytes {
		return Token{}, errInvalidToken
	}

	var secret [secretBytes]byte
	decodedSecretBytes, err := encoding.Decode(secret[:], []byte(secretText))
	if err != nil || decodedSecretBytes != secretBytes {
		return Token{}, errInvalidToken
	}

	return Token{Plaintext: raw, PublicID: publicID, Secret: secret[:]}, nil
}

// Digest computes the HMAC-SHA-256 digest of the complete credential.
func Digest(pepper []byte, raw string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(raw))

	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}
