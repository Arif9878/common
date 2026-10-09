package jwtauth

import "testing"

// FuzzParseJWKS feeds arbitrary documents to the JWKS parser: it must not
// panic, and every key it returns must carry a public key.
func FuzzParseJWKS(f *testing.F) {
	f.Add([]byte(`{"keys":[{"kty":"RSA","kid":"a","n":"AQAB","e":"AQAB"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"EC","crv":"P-256","kid":"b","x":"AA","y":"AA"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"c","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"oct","k":"c2VjcmV0"},{"kty":"RSA","use":"enc"}]}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, b []byte) {
		keys, err := parseJWKS(b)
		if err != nil {
			return
		}
		if len(keys) == 0 {
			t.Fatal("no error and no keys")
		}
		for kid, k := range keys {
			if k.pub == nil {
				t.Fatalf("key %q has no public key", kid)
			}
		}
	})
}
