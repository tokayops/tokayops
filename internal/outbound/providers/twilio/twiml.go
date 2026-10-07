package twilio

import (
	"encoding/xml"
	"strings"
)

// Say is the TwiML of a call that speaks text and hangs up.
func Say(language, text string) string {
	var b strings.Builder
	b.WriteString(`<Response><Say language="`)
	_ = xml.EscapeText(&b, []byte(language))
	b.WriteString(`">`)
	_ = xml.EscapeText(&b, []byte(text))
	b.WriteString(`</Say></Response>`)
	return b.String()
}

// SpokenDigits spells a code for the voice: "1, 2, 3, 4". Read as a number,
// "123456" is a hundred and twenty-three thousand.
func SpokenDigits(code string) string {
	parts := make([]string, 0, len(code))
	for _, r := range code {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, ", ")
}
