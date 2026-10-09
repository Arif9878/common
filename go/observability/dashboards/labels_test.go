package dashboards_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reservedAttr matches an attribute named like a label Prometheus sets on
// every scraped series. The scrape renames such a label to exported_job or
// exported_instance, so queries "by (job)" would group by the service
// instead of the attribute.
var reservedAttr = regexp.MustCompile(`attribute\.\w+\("(job|instance)"`)

func TestNoReservedPrometheusLabels(t *testing.T) {
	root := filepath.Join("..", "..") // the go/ directory
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // files of this repository
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if m := reservedAttr.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d: attribute %q becomes exported_%s in Prometheus; name it after what it is, such as %s.name",
					path, i+1, m[1], m[1], m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
