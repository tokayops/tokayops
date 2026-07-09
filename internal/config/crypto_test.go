package config

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	// Generate a valid 32-byte key
	key := make([]byte, KeyLength)
	for i := range key {
		key[i] = byte(i)
	}

	t.Run("roundtrip", func(t *testing.T) {
		plaintext := []byte(`{"token":"xoxb-secret","channel":"C123"}`)

		ciphertext, err := Encrypt(plaintext, key)
		if err != nil {
			t.Fatalf("Encrypt failed: %v", err)
		}

		if bytes.Equal(plaintext, ciphertext) {
			t.Error("Ciphertext should not equal plaintext")
		}

		decrypted, err := Decrypt(ciphertext, key)
		if err != nil {
			t.Fatalf("Decrypt failed: %v", err)
		}

		if !bytes.Equal(plaintext, decrypted) {
			t.Errorf("Decrypted text != original: got %s, want %s", decrypted, plaintext)
		}
	})

	t.Run("different ciphertext each time", func(t *testing.T) {
		plaintext := []byte("same input")

		ct1, _ := Encrypt(plaintext, key)
		ct2, _ := Encrypt(plaintext, key)

		if bytes.Equal(ct1, ct2) {
			t.Error("Same plaintext should produce different ciphertext (random nonce)")
		}
	})

	t.Run("invalid key size", func(t *testing.T) {
		shortKey := make([]byte, 16)

		_, err := Encrypt([]byte("test"), shortKey)
		if err != ErrInvalidKeySize {
			t.Errorf("Expected ErrInvalidKeySize, got %v", err)
		}

		_, err = Decrypt([]byte("test"), shortKey)
		if err != ErrInvalidKeySize {
			t.Errorf("Expected ErrInvalidKeySize, got %v", err)
		}
	})

	t.Run("tampered ciphertext fails", func(t *testing.T) {
		plaintext := []byte("secret")
		ciphertext, _ := Encrypt(plaintext, key)

		// Tamper with ciphertext
		ciphertext[len(ciphertext)-1] ^= 0xff

		_, err := Decrypt(ciphertext, key)
		if err == nil {
			t.Error("Expected error for tampered ciphertext")
		}
	})

	t.Run("ciphertext too short", func(t *testing.T) {
		_, err := Decrypt([]byte("short"), key)
		if err == nil {
			t.Error("Expected error for short ciphertext")
		}
	})
}

func TestGetEncryptionKey(t *testing.T) {
	t.Run("no env var", func(t *testing.T) {
		os.Unsetenv(EncryptionKeyEnv)
		_, err := GetEncryptionKey()
		if err != ErrNoEncryptionKey {
			t.Errorf("Expected ErrNoEncryptionKey, got %v", err)
		}
	})

	t.Run("valid key", func(t *testing.T) {
		validKey := make([]byte, 32)
		for i := range validKey {
			validKey[i] = byte(i)
		}
		os.Setenv(EncryptionKeyEnv, hex.EncodeToString(validKey))
		defer os.Unsetenv(EncryptionKeyEnv)

		key, err := GetEncryptionKey()
		if err != nil {
			t.Fatalf("GetEncryptionKey failed: %v", err)
		}
		if !bytes.Equal(key, validKey) {
			t.Error("Returned key doesn't match")
		}
	})

	t.Run("invalid hex", func(t *testing.T) {
		os.Setenv(EncryptionKeyEnv, "not-hex")
		defer os.Unsetenv(EncryptionKeyEnv)

		_, err := GetEncryptionKey()
		if err == nil {
			t.Error("Expected error for invalid hex")
		}
	})

	t.Run("wrong size", func(t *testing.T) {
		os.Setenv(EncryptionKeyEnv, hex.EncodeToString(make([]byte, 16)))
		defer os.Unsetenv(EncryptionKeyEnv)

		_, err := GetEncryptionKey()
		if err != ErrInvalidKeySize {
			t.Errorf("Expected ErrInvalidKeySize, got %v", err)
		}
	})
}
