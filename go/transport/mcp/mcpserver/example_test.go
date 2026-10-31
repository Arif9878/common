package mcpserver_test

import (
	"context"
	"log"
	"net/http"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/mcp/mcpserver"
)

type GetInvoiceIn struct {
	ID string `json:"id" jsonschema:"the invoice ID, such as inv_123"`
}

type Invoice struct {
	ID     string `json:"id"`
	Status string `json:"status" jsonschema:"draft, open, paid or void"`
	Total  int64  `json:"total" jsonschema:"in minor units of currency"`
}

func Example() {
	ctx := context.Background()
	// Tokens for this server name it as their audience; WithJWT checks
	// that, whatever the verifier's own Config.Audience says.
	v, err := jwtauth.New(ctx, jwtauth.Config{Issuer: "https://auth.example.com"}, jwtauth.WithAudiencePerCall())
	if err != nil {
		log.Fatal(err)
	}
	srv, err := mcpserver.New(mcpserver.Config{
		Name: "billing", Version: "1.4.0", Title: "Billing",
		Resource:             "https://api.example.com/mcp",
		AuthorizationServers: []string{"https://auth.example.com"},
	}, mcpserver.WithJWT(v))
	if err != nil {
		log.Fatal(err)
	}
	mcpserver.AddTool(srv, mcpserver.Tool[GetInvoiceIn, Invoice]{
		Name:        "get_invoice",
		Description: "Get one of the caller's invoices: its status and total.",
		Scopes:      []string{"invoice:read"},
		ReadOnly:    true,
		Handle: func(ctx context.Context, in GetInvoiceIn) (Invoice, error) {
			caller, _ := jwtauth.FromContext(ctx)
			_ = caller.Subject // look the invoice up among the caller's own
			return Invoice{}, errors.WithPublicMessage(errors.NotFound.New("invoice "+in.ID), "no such invoice")
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", srv.Handler())
	mux.Handle(srv.MetadataPath(), srv.MetadataHandler())
}
