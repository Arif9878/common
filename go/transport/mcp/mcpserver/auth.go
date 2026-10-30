package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// WithJWT makes every request authenticate with a JWT access token
// verified by v whose audience is Config.Resource (RFC 8707), whatever
// v's own Config.Audience says. Build v with jwtauth.WithAudiencePerCall
// if it serves only this server.
func WithJWT(v *jwtauth.Verifier) Option { return func(o *options) { o.jwt = v } }

func jwtAuthenticator(v *jwtauth.Verifier, resource string) Authenticator {
	audience := []string{resource}
	return func(ctx context.Context, token string) (context.Context, error) {
		return v.AuthenticateAudience(ctx, token, audience)
	}
}

var errNoToken = errors.WithPublicMessage(errors.Unauthorized.New("mcpserver: no bearer token"), "missing bearer token")

// authenticate checks the bearer token, then the scopes of the tool a
// tools/call names, before the SDK reads the request.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := jwtauth.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			s.challenge(w, r, errNoToken, false, nil)
			return
		}
		ctx, err := s.o.auth(r.Context(), token)
		if err != nil {
			if k := errors.KindOf(err); k != errors.Unauthorized && k != errors.Unavailable {
				err = errors.Unauthorized.Wrap(err, "mcpserver: authentication failed")
			}
			s.challenge(w, r, err, true, nil)
			return
		}
		r = r.WithContext(ctx)
		if scopes, err := s.checkCall(w, r); err != nil {
			s.challenge(w, r, err, true, scopes)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCall reads the body of a POST and, if it is a tools/call, checks
// the token's scopes against the tool's, returning them on failure so the
// client can ask for them (step-up authorization). The body is put back
// for the SDK. Anything else is left to the SDK to answer.
func (s *Server) checkCall(w http.ResponseWriter, r *http.Request) ([]string, error) {
	if r.Method != http.MethodPost {
		return nil, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcp.DefaultMaxRequestBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, err
		}
		return nil, errors.InvalidArgument.Wrap(err, "mcpserver: reading the request")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Method != "tools/call" {
		return nil, nil
	}
	scopes, ok := s.toolScopes(msg.Params.Name)
	if !ok {
		return nil, nil
	}
	return scopes, jwtauth.CheckScopes(r.Context(), scopes...)
}

// challenge answers a refused request: 401 or 403 with the
// WWW-Authenticate header MCP clients follow (RFC 6750 §3, RFC 9728 §5.1),
// other errors with their own status. The body is a problem document.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request, err error, hadToken bool, scopes []string) {
	params := []string{`resource_metadata="` + s.metadataURL() + `"`}
	switch errors.KindOf(err) {
	case errors.Unauthorized:
		if hadToken {
			params = append(params, `error="invalid_token"`)
		}
		w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	case errors.Forbidden:
		params = append(params, `error="insufficient_scope"`, `scope="`+strings.Join(scopes, " ")+`"`)
		w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	}
	httpserver.WriteError(w, r, err)
}
