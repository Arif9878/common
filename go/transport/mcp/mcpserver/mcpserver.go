package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/auth/jwtauth"
)

// MetadataPrefix is the well-known path of protected resource metadata
// (RFC 9728 §3); [Server.MetadataPath] appends the resource's path.
const MetadataPrefix = "/.well-known/oauth-protected-resource"

// Config describes the server to clients.
type Config struct {
	// Name and Version identify the server (MCP serverInfo). Required.
	Name    string
	Version string
	// Title is a name for people, such as "Orders"; Name is used without
	// it.
	Title string
	// Instructions tell the model how to use the tools together, if the
	// descriptions of the tools are not enough.
	Instructions string
	// Resource is the canonical URL of the MCP endpoint, such as
	// https://api.example.com/mcp: what clients connect to and what tokens
	// must be issued for. https, or http on a loopback address for local
	// development. Required.
	Resource string
	// AuthorizationServers are the issuers clients get tokens from
	// (RFC 9728 authorization_servers). Required with [WithAuth] or
	// [WithJWT].
	AuthorizationServers []string
	// Documentation is a URL describing the server for developers
	// (RFC 9728 resource_documentation).
	Documentation string
}

// Authenticator verifies a bearer token (without the "Bearer " prefix) and
// returns ctx carrying the caller, as jwtauth.Verifier.Authenticate does.
// The scopes of tools are checked against the jwtauth.Claims in that
// context. Errors should have kind Unauthorized, or Unavailable when the
// token cannot be checked now; others are treated as Unauthorized.
type Authenticator func(ctx context.Context, token string) (context.Context, error)

// Option configures [New].
type Option func(*options)

type options struct {
	auth       Authenticator
	jwt        *jwtauth.Verifier
	logger     *slog.Logger
	meterProv  metric.MeterProvider
	tracerProv trace.TracerProvider
}

// WithAuth makes every request authenticate with authenticate. Prefer
// [WithJWT] for JWT access tokens; WithAuth is for tokens it cannot verify,
// such as a service's own opaque tokens.
func WithAuth(authenticate Authenticator) Option { return func(o *options) { o.auth = authenticate } }

// WithLogger sets the logger for tool calls. Default: slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. Default: the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// WithTracerProvider sets the tracer provider. Default: the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// Server is an MCP server. Add tools with [AddTool], then serve
// [Server.Handler] at the path of Config.Resource.
type Server struct {
	cfg      Config
	resource *url.URL
	o        options
	sdk      *mcp.Server
	handler  http.Handler

	mu     sync.RWMutex
	scopes map[string][]string // tool name → required scopes

	duration metric.Float64Histogram
	tracer   trace.Tracer
}

// New returns a server for cfg. It fails if cfg is incomplete.
func New(cfg Config, opts ...Option) (*Server, error) {
	o := options{logger: slog.Default(), meterProv: otel.GetMeterProvider(), tracerProv: otel.GetTracerProvider()}
	for _, opt := range opts {
		opt(&o)
	}
	if o.jwt != nil {
		o.auth = jwtAuthenticator(o.jwt, cfg.Resource)
	}
	if cfg.Name == "" || cfg.Version == "" {
		return nil, fmt.Errorf("mcpserver: Name and Version are required")
	}
	resource, err := checkURL(cfg.Resource)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: Resource: %w", err)
	}
	if o.auth != nil && len(cfg.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("mcpserver: AuthorizationServers are required with authentication")
	}
	for _, as := range cfg.AuthorizationServers {
		if _, err := checkURL(as); err != nil {
			return nil, fmt.Errorf("mcpserver: AuthorizationServers: %w", err)
		}
	}

	s := &Server{cfg: cfg, resource: resource, o: o, scopes: map[string][]string{}}
	s.sdk = mcp.NewServer(&mcp.Implementation{Name: cfg.Name, Version: cfg.Version, Title: cfg.Title},
		&mcp.ServerOptions{Instructions: cfg.Instructions, Logger: o.logger})
	meter := o.meterProv.Meter("github.com/Arif9878/common/go/transport/mcp/mcpserver")
	s.duration, _ = meter.Float64Histogram("mcp.server.operation.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP requests handled by the server."))
	s.tracer = o.tracerProv.Tracer("github.com/Arif9878/common/go/transport/mcp/mcpserver")

	var h http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.sdk },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			Logger:                       o.logger,
			PropagateRequestCancellation: true,
		})
	if o.auth != nil {
		h = s.authenticate(h)
	}
	s.handler = h
	return s, nil
}

// checkURL accepts absolute https URLs without a fragment, and http ones
// on a loopback address (local development).
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q is not an absolute URL without a fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("%q must use https (http only on a loopback address)", raw)
}

// Handler serves the MCP endpoint (Streamable HTTP, stateless). Mount it
// at the path of Config.Resource.
func (s *Server) Handler() http.Handler { return s.handler }

// MetadataPath is where [Server.MetadataHandler] belongs: MetadataPrefix
// followed by the path of Config.Resource, such as
// /.well-known/oauth-protected-resource/mcp.
func (s *Server) MetadataPath() string {
	if s.resource.Path == "/" {
		return MetadataPrefix
	}
	return MetadataPrefix + s.resource.EscapedPath()
}

// metadataURL is the absolute URL of the metadata document, for
// WWW-Authenticate.
func (s *Server) metadataURL() string {
	return (&url.URL{Scheme: s.resource.Scheme, Host: s.resource.Host, Path: s.MetadataPath()}).String()
}

// MetadataHandler serves the protected resource metadata (RFC 9728) at
// [Server.MetadataPath]: the resource, its authorization servers and the
// scopes of every tool. Without authentication it answers 404, since
// there is nothing to authorize.
func (s *Server) MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.o.auth == nil {
			http.NotFound(w, r)
			return
		}
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               s.cfg.Resource,
			AuthorizationServers:   s.cfg.AuthorizationServers,
			ScopesSupported:        s.scopesSupported(),
			BearerMethodsSupported: []string{"header"},
			ResourceName:           cmp.Or(s.cfg.Title, s.cfg.Name),
			ResourceDocumentation:  s.cfg.Documentation,
		}).ServeHTTP(w, r)
	})
}

// scopesSupported is every scope a tool requires, sorted.
func (s *Server) scopesSupported() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []string
	for _, scopes := range s.scopes {
		for _, sc := range scopes {
			if !slices.Contains(all, sc) {
				all = append(all, sc)
			}
		}
	}
	slices.Sort(all)
	return all
}

// toolScopes returns the scopes tool requires and whether it exists.
func (s *Server) toolScopes(tool string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	scopes, ok := s.scopes[tool]
	return scopes, ok
}
