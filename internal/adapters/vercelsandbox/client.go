package vercelsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Errors the client distinguishes, so callers can decide without reading
// status codes.
var (
	errNotFound = errors.New("vercel: not found")
	errConflict = errors.New("vercel: conflict")
	// errStillWaiting means a request with a poll bound was still blocked
	// when the bound was reached: for a command read with wait=true, the
	// command is still running.
	errStillWaiting = errors.New("vercel: still waiting")
)

// Retry bounds for one call. Temporal retries the activity above this; these
// only absorb a brief rate limit or a transient gateway error, so an activity
// attempt is not spent on one.
const (
	maxAttempts     = 4
	maxRetryBackoff = 10 * time.Second
	callTimeout     = 60 * time.Second
)

// client speaks Vercel's REST API. There is no Go SDK; the operations used
// here and their parameter names were read from Vercel's OpenAPI document
// (https://openapi.vercel.sh), not the JS SDK — the spike found at least one
// named differently at the HTTP layer (`project`, not `projectId`, on list).
type client struct {
	http    *http.Client
	base    string
	token   string
	teamID  string
	sleep   func(context.Context, time.Duration) error
	timeout time.Duration
}

// request is one API call.
type request struct {
	method string
	path   string
	query  url.Values
	// body is JSON-encoded unless raw is set.
	body        any
	raw         []byte
	contentType string
	header      http.Header
	// accept lists the statuses that are success.
	accept []int
	// poll, when set, bounds a request expected to block — a command read
	// with wait=true — and reaching it is errStillWaiting, not a failure to
	// retry.
	poll time.Duration
	// sensitive marks a request whose body carries secrets — the runner's
	// start command. Its errors keep Vercel's error code but never its
	// message, in case a message ever echoed the request.
	sensitive bool
}

// apiError is the shape Vercel returns on failure.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do makes one call, retrying a rate limit or a transient gateway error.
//
// **Never includes the request body in an error.** The start command's body
// carries the runner's secrets in its environment; an error message that
// quoted it would put them in a log.
func (c *client) do(ctx context.Context, req request, out any) error {
	query := url.Values{}
	for k, v := range req.query {
		query[k] = v
	}
	query.Set("teamId", c.teamID)
	target := c.base + req.path + "?" + query.Encode()

	var payload []byte
	contentType := req.contentType
	switch {
	case req.raw != nil:
		payload = req.raw
	case req.body != nil:
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return fmt.Errorf("vercel: encode %s %s: %w", req.method, req.path, err)
		}
		payload, contentType = encoded, "application/json"
	}

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		status, body, retryAfter, err := c.once(ctx, req, target, payload, contentType)
		if err == nil {
			for _, ok := range req.accept {
				if status == ok {
					if out == nil || len(body) == 0 {
						return nil
					}
					if err := json.Unmarshal(body, out); err != nil {
						return fmt.Errorf("vercel: decode %s %s: %w", req.method, req.path, err)
					}
					return nil
				}
			}
			last = classify(req, status, body)
			if !retryable(req.method, status) {
				return last
			}
		} else if req.poll > 0 && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return errStillWaiting
		} else {
			// A transport error: the request may or may not have arrived.
			// Only an idempotent method is retried here; a POST is retried
			// by the activity, whose steps are idempotent by name.
			last = fmt.Errorf("vercel: %s %s: %w", req.method, req.path, err)
			if ctx.Err() != nil || (req.method != http.MethodGet && req.method != http.MethodDelete) {
				return last
			}
		}
		if attempt == maxAttempts {
			break
		}
		wait := retryAfter
		if wait <= 0 {
			wait = time.Duration(attempt) * time.Second
		}
		if err := c.sleep(ctx, min(wait, maxRetryBackoff)); err != nil {
			return last
		}
	}
	return last
}

func (c *client) once(ctx context.Context, req request, target string, payload []byte, contentType string) (int, []byte, time.Duration, error) {
	timeout := c.timeout
	if req.poll > 0 {
		timeout = req.poll
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(callCtx, req.method, target, body)
	if err != nil {
		return 0, nil, 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	for k, vs := range req.header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, 0, err
	}
	var retryAfter time.Duration
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
	}
	return resp.StatusCode, raw, retryAfter, nil
}

// retryable: a rate limit always; a gateway error only for a method safe to
// repeat.
func retryable(method string, status int) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return method == http.MethodGet || method == http.MethodDelete
	}
	return false
}

// classify turns a failed status into an error the backend can act on.
//
// Credentials, plan and payment refusals are **terminal**: the same request
// will be refused again, so they are application.ErrRunnerBackendRefused and
// the session fails naming the backend. Hobby pauses creation once its
// monthly allowance is spent; that arrives here too, and reads as a refusal,
// not a defect.
func classify(req request, status int, body []byte) error {
	var parsed apiError
	_ = json.Unmarshal(body, &parsed)
	detail := parsed.Error.Code
	if !req.sensitive {
		detail = strings.TrimSpace(detail + " " + parsed.Error.Message)
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	where := fmt.Sprintf("vercel %s %s returned %d", req.method, req.path, status)
	if detail != "" {
		where += ": " + detail
	}
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w (%s)", errNotFound, where)
	case http.StatusConflict:
		return fmt.Errorf("%w (%s)", errConflict, where)
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return fmt.Errorf("%w: %s", application.ErrRunnerBackendRefused, where)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// A request Vercel will not accept. Ours are built, never taken from
		// input, so this is a defect or an API change — not worth retrying.
		return fmt.Errorf("%w: %s", application.ErrRunnerBackendRefused, where)
	}
	return errors.New(where)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
