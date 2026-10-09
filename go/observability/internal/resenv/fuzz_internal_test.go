package resenv

import "testing"

// FuzzParse checks that malformed OTEL_RESOURCE_ATTRIBUTES never panic and
// never yield an empty key.
func FuzzParse(f *testing.F) {
	f.Add("service.name=orders,service.version=1.4.2")
	f.Add("a=%zz,=b,,c")
	f.Add("deployment.environment.name=prod%20eu")
	f.Fuzz(func(t *testing.T, s string) {
		for k := range parse(s) {
			if k == "" {
				t.Fatalf("empty key from %q", s)
			}
		}
	})
}
