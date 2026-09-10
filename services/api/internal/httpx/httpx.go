// Package httpx holds the transport concerns shared by every API route:
// request identity, the stable error envelope, and JSON writing.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// Stable, machine-readable error codes. These are part of the API contract:
// clients branch on them, so they are renamed only with a version change.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeInternal        = "internal_error"
	CodeNotFound        = "not_found"
)

// requestIDHeader is both read from the client and echoed on the response, so
// a caller can correlate its own logs with ours.
const requestIDHeader = "X-Request-Id"

// maxClientRequestIDLen bounds an inbound request ID. It is attacker-supplied
// data that ends up in logs; without a cap it is a log-flooding primitive.
const maxClientRequestIDLen = 128

type contextKey int

const requestIDKey contextKey = iota

// ErrorBody is the response shape for every API error.
//
// Documented in contracts/openapi/openapi.yaml. It deliberately carries no
// stack trace, driver error or internal hostname.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// WithRequestID assigns each request an identifier, exposes it on the
// response, and puts it in the context for logging and error bodies.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = uuid.NewString()
		}

		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestID returns the identifier assigned to this request, if any.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// sanitizeRequestID accepts a client-supplied identifier only when it is
// bounded and printable ASCII, so it cannot smuggle newlines or control
// characters into structured logs.
func sanitizeRequestID(value string) string {
	if value == "" || len(value) > maxClientRequestIDLen {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return value
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(ctx context.Context, w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be recorded.
		slog.ErrorContext(ctx, "failed to encode response body",
			slog.String("error", err.Error()),
			slog.String("request_id", RequestID(ctx)),
		)
	}
}

// WriteError writes the standard error envelope.
//
// The message is written for the caller and must stay free of internal
// detail; the diagnosis belongs in the logs, keyed by the same request ID.
func WriteError(ctx context.Context, w http.ResponseWriter, status int, code, message string) {
	WriteJSON(ctx, w, status, ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: RequestID(ctx),
	})
}
