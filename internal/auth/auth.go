package auth

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var (
	// JWTSecret is the key used to sign tokens.
	// In production, this should be loaded from environment variables.
	JWTSecret []byte

	// SessionDuration controls JWT and cookie expiration.
	// Default: 7 days. Override with SESSION_DURATION env var (Go duration, e.g. "168h").
	SessionDuration = 7 * 24 * time.Hour
)

func init() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		if os.Getenv("APP_ENV") == "production" {
			panic("FATAL: JWT_SECRET environment variable must be set in production")
		}
		// Dev default
		secret = "dev-secret-key-change-me"
	}
	JWTSecret = []byte(secret)

	if dur := os.Getenv("SESSION_DURATION"); dur != "" {
		parsed, err := ParseSessionDuration(dur)
		if err != nil {
			panic(fmt.Sprintf("FATAL: %v", err))
		}
		SessionDuration = parsed
	}
}

// ParseSessionDuration parses and validates a session duration string.
// Returns an error for unparseable, zero, or negative values.
func ParseSessionDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid SESSION_DURATION %q: %v", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("SESSION_DURATION must be positive, got %q", s)
	}
	return d, nil
}

// Claims defines the custom claims structure for our JWT.
type Claims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

// ValidatePassword checks if the password meets complexity requirements.
func ValidatePassword(password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters long")
	}

	var (
		hasUpper   bool
		hasLower   bool
		hasNumber  bool
		hasSpecial bool
	)

	for _, char := range password {
		switch {
		case unicode.IsUpper(char):
			hasUpper = true
		case unicode.IsLower(char):
			hasLower = true
		case unicode.IsNumber(char):
			hasNumber = true
		case unicode.IsPunct(char) || unicode.IsSymbol(char):
			hasSpecial = true
		}
	}

	if !hasUpper {
		return errors.New("password must contain at least one uppercase letter")
	}
	if !hasLower {
		return errors.New("password must contain at least one lowercase letter")
	}
	if !hasNumber {
		return errors.New("password must contain at least one number")
	}
	if !hasSpecial {
		return errors.New("password must contain at least one special character")
	}

	return nil
}

// HashPassword hashes a plain text password using bcrypt.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 14)
	return string(bytes), err
}

// CheckPasswordHash checks if the provided password matches the hash.
func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// GenerateToken generates a new JWT token for a user.
func GenerateToken(userID string) (string, error) {
	expirationTime := time.Now().Add(SessionDuration)
	claims := &Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "tokay",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(JWTSecret)
}

// ValidateToken parses and validates a JWT token.
func ValidateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return JWTSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	return claims, nil
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
