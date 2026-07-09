package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// EncryptionKeyEnv is the environment variable for the encryption key
	EncryptionKeyEnv = "ENCRYPTION_KEY"
	// KeyLength is the required key length for AES-256 (32 bytes = 256 bits)
	KeyLength = 32
)

var (
	ErrNoEncryptionKey = errors.New("ENCRYPTION_KEY environment variable not set")
	ErrInvalidKeySize  = errors.New("encryption key must be 32 bytes (64 hex characters)")
)

// GetEncryptionKey loads and validates the encryption key from environment
func GetEncryptionKey() ([]byte, error) {
	keyHex := os.Getenv(EncryptionKeyEnv)
	if keyHex == "" {
		return nil, ErrNoEncryptionKey
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid hex encoding for encryption key: %w", err)
	}

	if len(key) != KeyLength {
		return nil, ErrInvalidKeySize
	}

	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM
func Encrypt(plaintext []byte, key []byte) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, ErrInvalidKeySize
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Seal appends the encrypted data to nonce
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext using AES-256-GCM
func Decrypt(ciphertext []byte, key []byte) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, ErrInvalidKeySize
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, encrypted := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	return plaintext, nil
}
