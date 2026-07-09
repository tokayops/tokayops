package auth

import (
	"testing"
	"time"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"Short password", "Short1!", true},
		{"No uppercase", "short1!a", true},
		{"No lowercase", "SHORT1!A", true},
		{"No number", "ShortAa!", true},
		{"No special", "Short1Aa", true},
		{"Valid password", "Valid1!a", false},
		{"Valid long password", "ThisIsAVeryLongPassword123!@#", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePassword(tt.password); (err != nil) != tt.wantErr {
				t.Errorf("ValidatePassword() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHashAndCheckPassword(t *testing.T) {
	password := "TestPassword123!"

	// Test hashing
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if hash == "" {
		t.Error("HashPassword() returned empty hash")
	}
	if hash == password {
		t.Error("HashPassword() returned unhashed password")
	}

	// Test checking correct password
	if !CheckPasswordHash(password, hash) {
		t.Error("CheckPasswordHash() should return true for correct password")
	}

	// Test checking wrong password
	if CheckPasswordHash("WrongPassword123!", hash) {
		t.Error("CheckPasswordHash() should return false for wrong password")
	}
}

func TestGenerateAndValidateToken(t *testing.T) {
	userID := "testuser123"

	// Generate token
	token, err := GenerateToken(userID)
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if token == "" {
		t.Error("GenerateToken() returned empty token")
	}

	// Validate token
	claims, err := ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken() error = %v", err)
	}
	if claims.UserID != userID {
		t.Errorf("claims.UserID = %s, want %s", claims.UserID, userID)
	}
}

func TestValidateToken_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"Empty token", ""},
		{"Invalid format", "not.a.jwt"},
		{"Invalid signature", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1c2VyX2lkIjoidGVzdCJ9.invalidsignature"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateToken(tt.token)
			if err == nil {
				t.Error("ValidateToken() should return error for invalid token")
			}
		})
	}
}

func TestSessionDurationDefault(t *testing.T) {
	expected := 7 * 24 * time.Hour
	if SessionDuration != expected {
		t.Errorf("SessionDuration = %v, want %v", SessionDuration, expected)
	}
}

func TestParseSessionDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"valid hours", "2h", 2 * time.Hour, false},
		{"valid days", "168h", 7 * 24 * time.Hour, false},
		{"valid minutes", "30m", 30 * time.Minute, false},
		{"zero", "0s", 0, true},
		{"negative", "-1h", 0, true},
		{"invalid format", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSessionDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSessionDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseSessionDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
