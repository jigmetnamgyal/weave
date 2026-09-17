package workspaces

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// idempotencyHeader is the request header a caller supplies a key in.
const idempotencyHeader = "Idempotency-Key"

// retryAfterSeconds is what an in-flight caller is told to wait.
//
// A second, because that is long enough that the first attempt has usually
// finished and short enough that a person is not left staring at a form.
const retryAfterSeconds = 1

// idempotency holds what the transport needs to claim, replay and release.
type idempotency struct {
	keys application.IdempotencyRepository
}

// claimed is what a handler needs to know after a claim.
//
// `proceed` false means the request is already answered — either replayed or
// refused — and the handler must return without doing the work.
type claimed struct {
	proceed    bool
	completion *application.IdempotentCompletion
}

// claim takes the key for this request, answering the caller directly when the
// work has already been done or is still running.
//
// Called with the request body already decoded, because the fingerprint is
// over the request's *meaning* rather than its bytes: a client that reorders
// JSON members or sends a field empty where it omitted it before has not
// changed what it asked for, and comparing raw bytes would refuse its retry.
//
// A request with no key proceeds with no completion attached, which is how
// every caller behaved before this existed and must keep behaving.
func (i *idempotency) claim(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	handler *Handler,
	scope domain.IdempotencyScope,
	fingerprintFields []string,
) (claimed, bool) {
	raw := r.Header.Get(idempotencyHeader)
	if raw == "" {
		return claimed{proceed: true}, true
	}

	key, err := domain.ValidateIdempotencyKey(raw)
	if err != nil {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		return claimed{}, false
	}

	record := domain.IdempotencyRecord{
		Scope:       scope,
		Key:         key,
		Fingerprint: domain.FingerprintRequest(fingerprintFields...),
	}

	outcome, existing, claimant, err := i.keys.Claim(ctx, record, application.IdempotencyLease)
	if err != nil {
		handler.serverError(ctx, w, "claim idempotency key", err)
		return claimed{}, false
	}

	switch outcome {
	case application.IdempotencyClaimed:
		return claimed{
			proceed: true,
			completion: &application.IdempotentCompletion{
				Scope:           scope,
				Key:             key,
				Claimant:        claimant,
				OriginRequestID: httpx.RequestID(ctx),
			},
		}, true

	case application.IdempotencyComplete:
		i.replay(ctx, w, existing)
		return claimed{}, false

	case application.IdempotencyMismatch:
		httpx.WriteError(ctx, w, http.StatusUnprocessableEntity, httpx.CodeInvalidRequest,
			"This idempotency key was already used for a different request. Use a new key.")
		return claimed{}, false

	default: // IdempotencyInFlight
		// Refused rather than answered. An earlier attempt is still running,
		// and telling this caller "already done" would be a fabricated success
		// for work that may yet fail.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		httpx.WriteError(ctx, w, http.StatusConflict, httpx.CodeConflict,
			"An earlier request with this idempotency key is still running. Retry shortly.")
		return claimed{}, false
	}
}

// replay writes the stored response.
//
// The status and the body bytes exactly as first sent, with the headers that
// describe the body. `X-Request-Id` is deliberately *not* replayed: it is
// already set on this response by the middleware and identifies the request
// actually being answered, so an operator looking it up finds this exchange.
// The original is named in the log line below instead, which is what connects
// the two without lying about either.
func (i *idempotency) replay(ctx context.Context, w http.ResponseWriter, record domain.IdempotencyRecord) {
	if record.Response == nil {
		// Complete with no response stored cannot happen — the constraint
		// requires them together — so reaching here means the row was written
		// by something that bypassed it.
		httpx.WriteError(ctx, w, http.StatusInternalServerError, httpx.CodeInternal,
			"The stored response for this idempotency key is unreadable.")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(record.Response.Status)
	if _, err := w.Write(record.Response.Body); err != nil {
		// The status line is already sent, so this can only be recorded.
		slog.ErrorContext(ctx, "failed to write replayed response",
			slog.String("error", err.Error()),
			slog.String("request_id", httpx.RequestID(ctx)),
		)
		return
	}
	// The line that connects the retry to the call that did the work. Without
	// it the original request id is stored and unreachable.
	slog.InfoContext(ctx, "replayed an idempotent response",
		slog.String("request_id", httpx.RequestID(ctx)),
		slog.String("origin_request_id", record.Response.OriginRequestID),
	)
}

// release gives the key back after the work failed, so a caller whose request
// failed for an unrelated reason is not refused for the length of the lease.
func (i *idempotency) release(ctx context.Context, completion *application.IdempotentCompletion) {
	if completion == nil {
		return
	}
	if err := i.keys.Release(ctx, completion.Scope, completion.Key, completion.Claimant); err != nil {
		// Logged, not returned: the caller's request already failed for its
		// own reason, and the lease expiring covers this.
		slog.ErrorContext(ctx, "failed to release idempotency key",
			slog.String("error", err.Error()),
			slog.String("request_id", httpx.RequestID(ctx)),
		)
	}
}

// renderJSON produces the bytes to store for a response.
//
// `json.Marshal` rather than the encoder `WriteJSON` uses, because the encoder
// appends a newline and this must be the bytes a caller receives.
func renderJSON(status int, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return status, append(encoded, '\n'), nil
}
