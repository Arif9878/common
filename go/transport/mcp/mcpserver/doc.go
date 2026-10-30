// Package mcpserver serves a service's tools to AI clients over the Model
// Context Protocol (MCP), as an OAuth-protected resource.
//
//	srv, err := mcpserver.New(mcpserver.Config{
//		Name: "orders", Version: version,
//		Resource:             "https://api.example.com/mcp",
//		AuthorizationServers: []string{"https://auth.example.com"},
//	}, mcpserver.WithJWT(verifier), mcpserver.WithLogger(logger))
//	if err != nil {
//		return err
//	}
//	mcpserver.AddTool(srv, mcpserver.Tool[GetOrderIn, Order]{
//		Name: "get_order", Description: "Get one order by ID.",
//		Scopes: []string{"orders:read"}, ReadOnly: true,
//		Handle: orders.GetForTool,
//	})
//	mux.Handle("/mcp", srv.Handler())
//	mux.Handle(srv.MetadataPath(), srv.MetadataHandler())
//
// With Echo:
//
//	e.Any("/mcp", echo.WrapHandler(srv.Handler()))
//	e.GET(srv.MetadataPath(), echo.WrapHandler(srv.MetadataHandler()))
//
// The protocol itself is the official SDK's
// (github.com/modelcontextprotocol/go-sdk), served statelessly with
// Streamable HTTP, so any replica answers any request. This package adds
// what every service would otherwise write again: authorization, scopes per
// tool, error mapping and telemetry.
//
// # Experimental
//
// MCP still changes in incompatible ways between revisions. Until it
// settles, this package is experimental in the sense of COMPATIBILITY.md:
// its API and behavior may change in a minor release, and the release
// notes say how to upgrade.
//
// # Authorization
//
// The server is an OAuth 2.0 protected resource (RFC 9728), as the MCP
// authorization specification requires. With [WithJWT] or [WithAuth]:
//
//   - Every request needs an "Authorization: Bearer" token. Without one,
//     or with an invalid one, the answer is 401 with a WWW-Authenticate
//     header pointing at the metadata document, from which clients find
//     the authorization server.
//   - [WithJWT] accepts only tokens whose audience is Config.Resource
//     (RFC 8707), so a token issued for another API cannot be replayed
//     here, and the reverse.
//   - A tools/call for a tool whose scopes the token lacks is answered with
//     403 and error="insufficient_scope" naming them, so clients can ask
//     the user for more (step-up authorization). The tool checks again
//     before it runs.
//   - [Server.MetadataHandler] serves the metadata document: the resource,
//     its authorization servers and the scopes of every tool.
//
// Handlers read the caller with jwtauth.FromContext. Scopes only say what
// kind of operation a client may ask for; handlers still check that the
// caller may touch the data named in the input (its tenant, its
// organization), as every other handler does.
//
// # Errors
//
// A handler's error reaches the client as a tool result with isError set
// and only errors.PublicMessage as its text, prefixed by the kind, for
// example "not_found: no such order". The model reads it and can correct
// its input. The full error stays in the log. Panics become an Internal
// error.
//
// # Telemetry
//
// Each tool call gets a span "tools/call <tool>", the histogram
// mcp.server.operation.duration (seconds; mcp.method.name,
// gen_ai.tool.name, error.type for failures), and one log line "mcp tool
// call" with the tool, duration_ms and error. Inputs and outputs are never
// logged. Put the server behind httpserver.Handler or echoadapter.Middleware
// for the request ID, the access log and the parent span.
package mcpserver
