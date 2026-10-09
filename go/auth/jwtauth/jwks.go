package jwtauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/Arif9878/common/go/errors"
)

// key is one verification key of a JWKS.
type key struct {
	pub crypto.PublicKey
	alg string // "" if the JWK does not restrict it
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS returns the signing keys of a JWKS document by key ID. Keys
// for encryption, of unsupported types, or malformed are skipped, so one
// odd key does not take down verification with the others.
func parseJWKS(b []byte) (map[string]key, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	keys := map[string]key{}
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			continue
		}
		keys[k.Kid] = key{pub: pub, alg: k.Alg}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS has no usable signing keys (of %d)", len(doc.Keys))
	}
	return keys, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64int(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64int(k.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 {
			return nil, fmt.Errorf("invalid RSA exponent")
		}
		if n.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA key under 2048 bits")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		size := (curve.Params().BitSize + 7) / 8
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) != size || len(y) != size {
			return nil, fmt.Errorf("invalid EC coordinates")
		}
		// Rejects points not on the curve.
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, fmt.Errorf("invalid EC point: %w", err)
		}
		return pub, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid Ed25519 key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("invalid base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}

// fetch GETs url and returns its body, at most 1 MiB.
func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "jwtauth: "+url)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Unavailable.Wrap(err, "jwtauth: fetch "+url)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Unavailable.Errorf("jwtauth: fetch %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.Unavailable.Wrap(err, "jwtauth: read "+url)
	}
	return b, nil
}

// discover returns the jwks_uri of an OpenID Connect issuer.
func discover(ctx context.Context, client *http.Client, issuer string) (string, error) {
	b, err := fetch(ctx, client, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return "", err
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.JWKSURI == "" {
		return "", errors.Unavailable.New("jwtauth: OpenID configuration has no jwks_uri")
	}
	if doc.Issuer != issuer {
		// RFC 8414: the document must name the issuer it was fetched for.
		return "", errors.InvalidArgument.Errorf("jwtauth: OpenID configuration is for issuer %q, not %q", doc.Issuer, issuer)
	}
	return doc.JWKSURI, nil
}
