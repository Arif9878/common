package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
)

const pw = "hunter2-s3cr3t"

func TestFieldsAreCopied(t *testing.T) {
	fields := map[string]string{"username": "app", "password": pw}
	s := secret.New(fields)
	fields["password"] = "changed"
	if s.Field("password") != pw {
		t.Fatal("secret shares the caller's map")
	}
	if v, ok := s.Lookup("missing"); ok || v != "" {
		t.Error("Lookup of a missing field")
	}
	if got := strings.Join(s.Fields(), ","); got != "password,username" {
		t.Errorf("Fields = %q", got)
	}
}

func TestRequire(t *testing.T) {
	s := secret.New(map[string]string{"username": "app", "password": ""})
	err := s.Require("username", "password", "host")
	if errors.KindOf(err) != errors.InvalidArgument || !strings.Contains(err.Error(), "[password host]") {
		t.Fatalf("Require = %v", err)
	}
	if err := s.Require("username"); err != nil {
		t.Fatal(err)
	}
}

func TestNeverPrintsValues(t *testing.T) {
	s := secret.New(map[string]string{"password": pw})
	s.Version = "3"
	s.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	type holder struct{ S secret.Secret }

	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outs = append(outs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, holder{s}))
	}
	j, _ := json.Marshal(holder{s})
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "secret", s)
	outs = append(outs, buf.String())

	for _, out := range outs {
		if strings.Contains(out, pw) || strings.Contains(out, fmt.Sprintf("%x", pw)) {
			t.Errorf("value leaked: %s", out)
		}
	}
	if !strings.Contains(buf.String(), `"version":"3"`) || !strings.Contains(buf.String(), `"fields":["password"]`) {
		t.Errorf("log lacks metadata: %s", buf.String())
	}
}

func TestTTL(t *testing.T) {
	if (secret.Secret{}).TTL() != 0 {
		t.Error("non-expiring secret has a TTL")
	}
	s := secret.Secret{ExpiresAt: time.Now().Add(-time.Minute)}
	if s.TTL() >= 0 {
		t.Error("expired secret has a non-negative TTL")
	}
}

func TestStatic(t *testing.T) {
	var p secret.Provider = secret.Static{"kv/app": secret.New(map[string]string{"token": "t"})}
	s, err := p.Get(context.Background(), "kv/app")
	if err != nil || s.Field("token") != "t" {
		t.Fatalf("Get = %v, %v", s, err)
	}
	if _, err := p.Get(context.Background(), "kv/other"); !errors.Is(err, secret.ErrNotFound) || errors.KindOf(err) != errors.NotFound {
		t.Fatalf("missing key: %v", err)
	}

	f := secret.ProviderFunc(func(context.Context, string) (secret.Secret, error) {
		return secret.Secret{}, errors.Unavailable.New("down")
	})
	if _, err := f.Get(context.Background(), "x"); errors.KindOf(err) != errors.Unavailable {
		t.Fatal(err)
	}
}
