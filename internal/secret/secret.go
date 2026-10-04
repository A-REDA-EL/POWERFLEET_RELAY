// Package secret encrypts values stored at rest and signs session tokens,
// both keyed from RELAY_SECRET_KEY.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Box struct {
	aead    cipher.AEAD
	signKey []byte
}

func New(key string) (*Box, error) {
	if len(key) < 16 {
		return nil, errors.New("RELAY_SECRET_KEY must be at least 16 characters")
	}
	encKey := sha256.Sum256([]byte("relay-encryption:" + key))
	signKey := sha256.Sum256([]byte("relay-signing:" + key))
	block, err := aes.NewCipher(encKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, signKey: signKey[:]}, nil
}

func (b *Box) Encrypt(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (b *Box) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", fmt.Errorf("cannot decrypt stored secret (was RELAY_SECRET_KEY changed?): %w", err)
	}
	return string(plain), nil
}

// Token returns a session token valid until expiry.
func (b *Box) Token(expiry time.Time) string {
	payload := strconv.FormatInt(expiry.Unix(), 10)
	return payload + "." + b.sign(payload)
}

func (b *Box) ValidToken(token string) bool {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(b.sign(payload))) {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	return err == nil && time.Now().Unix() < exp
}

func (b *Box) sign(payload string) string {
	mac := hmac.New(sha256.New, b.signKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
