// Package cursor encodes keyset pagination cursors.
//
// Keyset pagination, not OFFSET: at ~9M active events an OFFSET deep into the result
// set makes Postgres walk every skipped row. A cursor carries the sort key of the last
// row seen, so each page is an index seek regardless of depth.
//
// The cursor is opaque but deliberately NOT signed. A forged cursor can only change
// which page of public search results the caller sees — no authorization decision
// depends on its contents, and it addresses no private data. Signing it would add key
// management for no security benefit. It is, however, strictly validated: a malformed
// cursor is a client error, never a 500 or a silent full scan.
package cursor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrMalformed indicates a cursor that could not be decoded. Callers should map this
// to a 400, not a 500.
var ErrMalformed = errors.New("malformed cursor")

// Key is the sort position of the last row on the previous page. It mirrors the
// ORDER BY (starts_at, event_id) used by the search query, and both fields are
// required for a total order — start times are not unique.
type Key struct {
	StartsAt time.Time `json:"s"`
	EventID  int64     `json:"e"`
}

// Encode renders a key as a URL-safe opaque string.
func Encode(k Key) (string, error) {
	if k.EventID <= 0 {
		return "", fmt.Errorf("%w: event id must be positive", ErrMalformed)
	}
	buf, err := json.Marshal(k)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Decode parses a cursor produced by Encode.
//
// Every failure path returns ErrMalformed so the caller has one thing to check, and
// the messages stay specific enough to debug against.
func Decode(s string) (Key, error) {
	if s == "" {
		return Key{}, fmt.Errorf("%w: empty", ErrMalformed)
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("%w: not valid base64url", ErrMalformed)
	}

	// DisallowUnknownFields keeps the wire format tight, so a cursor from a future
	// version fails loudly here rather than silently paginating from the wrong place.
	var k Key
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&k); err != nil {
		return Key{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	if k.EventID <= 0 {
		return Key{}, fmt.Errorf("%w: event id must be positive", ErrMalformed)
	}
	if k.StartsAt.IsZero() {
		return Key{}, fmt.Errorf("%w: missing start time", ErrMalformed)
	}
	return k, nil
}
