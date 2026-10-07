package twilio

import (
	"net/url"
	"testing"
)

// The worked example of Twilio's own documentation (usage/security), so the
// test does not check the algorithm against itself.
func TestTheSignatureIsTwiliosOwnExample(t *testing.T) {
	params := url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Caller":  {"+14158675310"},
		"Digits":  {"1234"},
		"From":    {"+14158675310"},
		"To":      {"+18005551212"},
	}
	const fullURL = "https://example.com/myapp.php?foo=1&bar=2"
	if got := Signature("12345", fullURL, params); got != "L/OH5YylLD5NRKLltdqwSvS0BnU=" {
		t.Fatalf("signature = %s", got)
	}
	if !ValidSignature("12345", fullURL, params, "L/OH5YylLD5NRKLltdqwSvS0BnU=") {
		t.Fatal("the documented signature does not validate")
	}
	if ValidSignature("12345", "https://example.com:443/myapp.php?foo=1&bar=2", params, "L/OH5YylLD5NRKLltdqwSvS0BnU=") {
		t.Fatal("a different URL validated")
	}
	if ValidSignature("", fullURL, params, "") {
		t.Fatal("no token, no signature, validated")
	}
}
