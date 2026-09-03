package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

type WorkspaceAuthenticator interface {
	AuthenticateAPIKey(context.Context, string) (string, error)
}

type SessionTokenAuthenticator interface {
	AuthenticateSessionToken(context.Context, string) (string, workspace.SessionScope, error)
}

type Config struct {
	RequireAuth   bool
	Authenticator WorkspaceAuthenticator
}

// maxBodyBytes is the documented request-size limit for Sessions, Agents, and
// Environments: 32 MiB. Exceeding it yields a 413 request_too_large.
const maxBodyBytes = 32 << 20
const maxFileRequestBytes = app.MaxFileBytes + (1 << 20)
const maxSkillRequestBytes = app.MaxSkillUploadBytes + (1 << 20)

func authMiddleware(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		key, present, validShape := requestBearerToken(r)
		if !validShape {
			writeErrorEnvelope(w, http.StatusUnauthorized, "authentication_error",
				"invalid authorization header")
			return
		}
		if cfg.Authenticator != nil {
			if !present {
				writeErrorEnvelope(w, http.StatusUnauthorized, "authentication_error",
					"missing authorization header")
				return
			}
			workspaceID, err := cfg.Authenticator.AuthenticateAPIKey(r.Context(), key)
			if err == nil {
				r = r.WithContext(workspace.WithScope(r.Context(), workspaceID))
			} else {
				if !errors.Is(err, workspace.ErrInvalidAPIKey) {
					writeError(w, err)
					return
				}
				sessionAuthenticator, ok := cfg.Authenticator.(SessionTokenAuthenticator)
				if !ok {
					writeErrorEnvelope(w, http.StatusUnauthorized, "authentication_error",
						"invalid API key")
					return
				}
				workspaceID, sessionScope, sessionErr := sessionAuthenticator.AuthenticateSessionToken(
					r.Context(), key,
				)
				if sessionErr != nil {
					if !errors.Is(sessionErr, workspace.ErrInvalidSessionToken) {
						writeError(w, sessionErr)
						return
					}
					writeErrorEnvelope(w, http.StatusUnauthorized, "authentication_error",
						"invalid credential")
					return
				}
				if !sessionScopeAllows(r, sessionScope) {
					writeErrorEnvelope(w, http.StatusForbidden, "permission_error",
						"session credential is not authorized for this resource")
					return
				}
				r = r.WithContext(workspace.WithSessionScope(r.Context(), workspaceID, sessionScope))
			}
		} else if cfg.RequireAuth && !present {
			writeErrorEnvelope(w, http.StatusUnauthorized, "authentication_error",
				"missing authorization header")
			return
		} else if _, ok := workspace.FromContext(r.Context()); !ok {
			// Embedders and wire-only tests that intentionally omit an
			// Authenticator retain the historical single-tenant behavior.
			r = r.WithContext(workspace.WithScope(r.Context(), workspace.DefaultID))
		}
		next.ServeHTTP(w, r)
	})
}

func sessionScopeAllows(r *http.Request, scope workspace.SessionScope) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" {
		return false
	}
	if len(parts) == 6 && parts[1] == "environments" &&
		parts[2] == scope.EnvironmentID && parts[3] == "work" && parts[4] == scope.WorkID &&
		r.Method == http.MethodPost {
		return parts[5] == "heartbeat" || parts[5] == "stop"
	}
	if parts[1] == "sessions" && parts[2] == scope.SessionID {
		switch {
		case len(parts) == 3 && r.Method == http.MethodGet:
			return true
		case len(parts) == 4 && parts[3] == "events" &&
			(r.Method == http.MethodGet || r.Method == http.MethodPost):
			return true
		case len(parts) == 5 && parts[3] == "events" && parts[4] == "stream" &&
			r.Method == http.MethodGet:
			return true
		}
	}
	if r.Method != http.MethodGet {
		return false
	}
	if len(parts) >= 3 && parts[1] == "files" {
		_, allowed := scope.Files[parts[2]]
		return allowed && (len(parts) == 3 || (len(parts) == 4 && parts[3] == "content"))
	}
	if len(parts) >= 3 && parts[1] == "skills" {
		if len(parts) == 3 {
			for skill := range scope.Skills {
				if skill.ID == parts[2] {
					return true
				}
			}
			return false
		}
		if len(parts) == 5 || (len(parts) == 6 && parts[5] == "content") {
			if parts[3] != "versions" {
				return false
			}
			_, allowed := scope.Skills[workspace.SkillVersion{ID: parts[2], Version: parts[4]}]
			return allowed
		}
	}
	return false
}

func isPublicRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/healthz" ||
		r.URL.Path == "/readyz" || r.URL.Path == "/openapi.yaml")
}

func requestBearerToken(r *http.Request) (key string, present bool, valid bool) {
	authorization := strings.TrimSpace(r.Header.Get("authorization"))
	if authorization == "" {
		return "", false, true
	}
	scheme, value, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(value) == "" {
		return "", true, false
	}
	return strings.TrimSpace(value), true, true
}

func contentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Body == nil || r.ContentLength == 0 {
			next.ServeHTTP(w, r)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("content-type"))
		requiredType := "application/json"
		if isMultipartUpload(r) {
			requiredType = "multipart/form-data"
		}
		if err != nil || mediaType != requiredType {
			writeErrorEnvelope(w, http.StatusBadRequest, "invalid_request_error",
				"content-type must be "+requiredType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ensureRequestID(w)
		next.ServeHTTP(w, r)
	})
}

// bodyLimitMiddleware rejects request bodies larger than the documented 32 MiB
// limit with a 413 request_too_large. It only inspects mutating methods that
// carry a body.
func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxBodyBytes)
		message := "request body exceeds 32 MiB limit"
		if isFileUpload(r) {
			limit = maxFileRequestBytes
			message = "file request exceeds 500 MB limit"
		} else if isSkillUpload(r) {
			limit = maxSkillRequestBytes
			message = "Skill upload must be smaller than 30 MB"
		}
		if r.ContentLength > limit {
			writeErrorEnvelope(w, http.StatusRequestEntityTooLarge, "request_too_large",
				message)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func isFilePath(path string) bool {
	return path == "/v1/files" || strings.HasPrefix(path, "/v1/files/")
}

func isFileUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/files"
}

func isSkillPath(path string) bool {
	return path == "/v1/skills" || strings.HasPrefix(path, "/v1/skills/")
}

func isSkillUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && isSkillPath(r.URL.Path) &&
		(r.URL.Path == "/v1/skills" || strings.HasSuffix(r.URL.Path, "/versions"))
}

func isMultipartUpload(r *http.Request) bool {
	return isFileUpload(r) || isSkillUpload(r)
}

// decodeJSONBody centralizes JSON parsing so known-length and chunked bodies
// receive the same 32 MiB behavior. It also rejects trailing JSON values and
// unknown top-level fields instead of silently accepting a wider API.
func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return domain.TooLarge("request body exceeds 32 MiB limit")
		}
		return domain.Validation("invalid JSON body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return domain.TooLarge("request body exceeds 32 MiB limit")
		}
		return domain.Validation("request body must contain exactly one JSON value")
	}
	return nil
}
