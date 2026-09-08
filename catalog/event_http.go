package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"encore.dev"
	"encore.dev/beta/errs"
	"encore.dev/rlog"
)

// maxAgeSeconds is how long a CDN or browser may reuse an event body without asking.
//
// It matches the availability TTL: the body embeds an advisory availability count, so
// permitting a longer reuse than the server's own snapshot would be dishonest.
const maxAgeSeconds = int(availabilityTTL / 1e9)

// GetEvent returns an event's public detail.
//
// This is a raw endpoint rather than a typed one for a single reason: HTTP validators.
// A typed Encore endpoint cannot return 304, and revalidation is the whole point of
// this layer — at 80k RPS the cheapest possible response is one with no body. Raw
// gives control over ETag, Cache-Control and the conditional-request path.
//
//encore:api public raw method=GET path=/v1/events/:eventID
func (s *Service) GetEvent(w http.ResponseWriter, req *http.Request) {
	eventID, ok := pathEventID(req)
	if !ok {
		writeErr(w, &errs.Error{Code: errs.InvalidArgument, Message: "invalid event id"})
		return
	}

	ev, err := s.eventDetail(req.Context(), eventID)
	if err != nil {
		writeErr(w, err)
		return
	}

	body, err := json.Marshal(ev)
	if err != nil {
		writeErr(w, err)
		return
	}

	// The validator covers the whole representation — event version *and* the
	// availability numbers. Deriving it from the version alone would be cheaper but
	// wrong: a 304 would then hide a sold-out section from a revalidating client,
	// which on a ticketing page is the one thing they came to check.
	etag := weakETag(body)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAgeSeconds))
	// Availability moves independently of the event body, so downstream caches must
	// not assume the age of one implies the age of the other.
	w.Header().Set("Vary", "Accept-Encoding")

	if matchesETag(req.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		rlog.Debug("client disconnected while writing event body", "err", err)
	}
}

// pathEventID reads the event id from the routed path.
func pathEventID(req *http.Request) (int64, bool) {
	raw := ""
	if r := encore.CurrentRequest(); r != nil {
		raw = r.PathParams.Get("eventID")
	}
	if raw == "" {
		// Fallback for any path where Encore has not populated params.
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) > 0 {
			raw = parts[len(parts)-1]
		}
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// weakETag is a weak validator: the body is semantically equivalent between two
// responses with the same tag, but byte equality is not promised across encodings.
func weakETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// matchesETag implements If-None-Match comparison.
//
// It accepts a comma-separated list and the "*" wildcard, and compares weak tags by
// ignoring the W/ prefix, which is what RFC 9110 calls weak comparison.
func matchesETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}

	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.TrimPrefix(candidate, "W/") == want {
			return true
		}
	}
	return false
}

// writeErr renders an error using the same envelope Encore's typed endpoints use, so
// clients see one error shape across the API.
func writeErr(w http.ResponseWriter, err error) {
	code := errs.Code(err)
	if code == errs.OK {
		code = errs.Internal
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code.HTTPStatus())

	message := "internal error"
	if e, ok := err.(*errs.Error); ok {
		message = e.Message
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    code.String(),
		"message": message,
		"details": nil,
	})
}
