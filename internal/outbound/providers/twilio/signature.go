package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// Signature is X-Twilio-Signature for a POST to fullURL with params: HMAC-SHA1,
// keyed by the account's auth token, over the URL Twilio called followed by
// every POST field sorted by name, name then value, with nothing between. The
// URL is the one Twilio called - with its query, without a fragment, without
// a port for HTTPS - and not one rebuilt from the request behind a proxy.
func Signature(authToken, fullURL string, params url.Values) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(fullURL)
	for _, name := range names {
		values := append([]string(nil), params[name]...)
		sort.Strings(values)
		for _, value := range values {
			b.WriteString(name)
			b.WriteString(value)
		}
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ValidSignature compares in constant time.
func ValidSignature(authToken, fullURL string, params url.Values, signature string) bool {
	if authToken == "" || signature == "" {
		return false
	}
	want := Signature(authToken, fullURL, params)
	return subtle.ConstantTimeCompare([]byte(want), []byte(signature)) == 1
}
