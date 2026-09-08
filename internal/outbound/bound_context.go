package outbound

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BoundContext is the third field of a generation, beside the endpoint and
// the create key: what a message takes from a NEIGHBOURING commitment.
//
// A direct message points back to the card in the channel, and the card's
// coordinates belong to another commitment. Read on the attempt, a retry of
// one generation would go out with different bytes under the same provider
// key - the card had no message yet on the first attempt and has one on the
// second. So the context is settled once, when the generation opens, and a
// new generation settles it again. The workspace's address is frozen with the
// coordinates for the same reason: it is the other half of the link, and a
// setting read on the attempt would be an input outside the snapshot, the
// payload and this.
type BoundContext struct {
	CardReceiptRef string `json:"card_receipt_ref,omitempty"`
	TeamURL        string `json:"team_url,omitempty"`
}

// Empty is a context with nothing in it, which is stored as NULL.
func (c BoundContext) Empty() bool { return c == BoundContext{} }

// Encode is the stored form, nil when there is nothing to store.
func (c BoundContext) Encode() (json.RawMessage, error) {
	if c.Empty() {
		return nil, nil
	}
	return json.Marshal(c)
}

// DecodeBoundContext reads a stored context strictly: it decides the bytes
// of a message, and a shape this build does not know is a refusal rather than
// an empty context.
func DecodeBoundContext(raw json.RawMessage) (BoundContext, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return BoundContext{}, nil
	}
	var c BoundContext
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return BoundContext{}, fmt.Errorf("the bound context cannot be read: %w", err)
	}
	return c, nil
}
