package query

import (
	"encoding/base64"

	"github.com/substrate/substrate/internal/jsonx"
)

// cursorData is the decoded payload of an opaque keyset cursor.
type cursorData struct {
	Sort  string `json:"s"`  // normalized sort spec the cursor was produced under
	Value string `json:"v"`  // last row's sort-key value, as a string
	ID    string `json:"id"` // last row's id (final tiebreaker)
}

func encodeCursor(c cursorData) string {
	b, _ := jsonx.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(tok string) (cursorData, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return cursorData{}, badRequest("invalid cursor encoding")
	}
	var c cursorData
	if err := jsonx.Unmarshal(raw, &c); err != nil {
		return cursorData{}, badRequest("invalid cursor payload")
	}
	return c, nil
}
