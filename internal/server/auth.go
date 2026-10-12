package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/limiter"
)

// Authenticator resolves the API key a request presents. auth.Cache is the
// implementation.
type Authenticator interface {
	Authenticate(ctx context.Context, presented string) (auth.Key, error)
}

// Limiter enforces each key's requests and tokens per minute.
// limiter.Registry is the in-process implementation; one shared between
// gateway instances, say on Redis, could replace it.
type Limiter interface {
	AllowRequest(keyID int64, l limiter.Limits) (retryAfter time.Duration, ok bool)
	ReserveTokens(keyID int64, l limiter.Limits, n int) (retryAfter time.Duration, ok bool)
	RefundTokens(keyID int64, l limiter.Limits, reserved, used int)
}

// keyFrom returns the key that authenticate stored in ctx.
func keyFrom(ctx context.Context) auth.Key {
	k, _ := ctx.Value(apiKeyKey).(auth.Key)
	return k
}

func limitsOf(k auth.Key) limiter.Limits {
	return limiter.Limits{RPM: k.RPM, TPM: k.TPM}
}

// authenticate resolves the request's key (pipeline step 3) and stores it in
// the request context. Every request under /v1/ needs one.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			unauthorized(w, "No API key provided. Send it in the Authorization header: `Authorization: Bearer <key>`.")
			return
		}
		key, err := s.keys.Authenticate(r.Context(), presented)
		switch {
		case errors.Is(err, auth.ErrUnknownKey):
			unauthorized(w, "Incorrect API key provided.")
			return
		case errors.Is(err, auth.ErrRevokedKey):
			unauthorized(w, "The API key has been revoked.")
			return
		case err != nil:
			if r.Context().Err() != nil {
				return // the client went away during the lookup
			}
			s.log.ErrorContext(r.Context(), "key lookup failed", "request_id", RequestID(r.Context()), "error", err)
			api.WriteError(w, &api.Error{
				Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: api.CodeAuthUnavailable,
				Message: "The gateway can't check API keys right now. Retry shortly.", RetryAfter: time.Second,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyKey, key)))
	})
}

// bearerToken returns the token from an "Authorization: Bearer <token>"
// header. The scheme is case-insensitive (RFC 7235).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	api.WriteError(w, &api.Error{
		Status: http.StatusUnauthorized, Type: api.TypeInvalidRequest, Code: api.CodeInvalidAPIKey, Message: msg,
	})
}

// limitRequests counts the request against its key's requests per minute
// (pipeline step 4), before the body is read.
func (s *Server) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFrom(r.Context())
		if wait, ok := s.limiter.AllowRequest(key.ID, limitsOf(key)); !ok {
			api.WriteError(w, &api.Error{
				Status: http.StatusTooManyRequests, Type: api.TypeRequests, Code: api.CodeRateLimitExceeded,
				Message: fmt.Sprintf("Rate limit reached for requests per minute (RPM): limit %d. "+
					"Retry after the time in the Retry-After header.", key.RPM),
				RetryAfter: wait,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// reserveTokens takes n tokens from the key's tokens per minute (pipeline
// step 7). It returns the error to send if they can't be had. A request
// needing more than the whole limit can never run, so it gets no
// Retry-After.
func (s *Server) reserveTokens(key auth.Key, n int) *api.Error {
	if n > key.TPM {
		return &api.Error{
			Status: http.StatusTooManyRequests, Type: api.TypeTokens, Code: api.CodeRateLimitExceeded,
			Message: fmt.Sprintf("Request too large for tokens per minute (TPM): limit %d, requested %d "+
				"(the estimated prompt plus max_tokens). Shorten the prompt or lower max_tokens.", key.TPM, n),
		}
	}
	if wait, ok := s.limiter.ReserveTokens(key.ID, limitsOf(key), n); !ok {
		return &api.Error{
			Status: http.StatusTooManyRequests, Type: api.TypeTokens, Code: api.CodeRateLimitExceeded,
			Message: fmt.Sprintf("Rate limit reached for tokens per minute (TPM): limit %d, requested %d. "+
				"Retry after the time in the Retry-After header.", key.TPM, n),
			RetryAfter: wait,
		}
	}
	return nil
}
