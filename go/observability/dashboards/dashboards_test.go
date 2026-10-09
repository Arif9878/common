package dashboards_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Arif9878/common/go/observability/dashboards"
)

var (
	quotedRE   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	selectorRE = regexp.MustCompile(`\{[^}]*\}`)
	rangeRE    = regexp.MustCompile(`\[[^\]]*\]`)
	groupingRE = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	identRE    = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	promql     = map[string]bool{
		"sum": true, "min": true, "max": true, "avg": true, "count": true, "rate": true, "irate": true,
		"increase": true, "histogram_quantile": true, "and": true, "or": true, "unless": true, "bool": true,
		"label_values": true, "clamp_min": true, "abs": true, "deriv": true, "absent": true, "offset": true,
	}
)

// metricsIn returns the metric names a PromQL expression reads.
func metricsIn(expr string) []string {
	e := quotedRE.ReplaceAllString(expr, "")
	e = selectorRE.ReplaceAllString(e, " ")
	e = rangeRE.ReplaceAllString(e, " ")
	e = groupingRE.ReplaceAllString(e, " ")
	var out []string
	for _, id := range identRE.FindAllString(e, -1) {
		if !promql[id] && !strings.HasPrefix(id, "__") {
			out = append(out, id)
		}
	}
	return out
}

func TestMetricsIn(t *testing.T) {
	got := metricsIn(`histogram_quantile(0.99, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket{job=~"$job", x="a{b}"}[$__rate_interval]))) / sum(up) or vector_x`)
	if !slices.Equal(got, []string{"http_server_request_duration_seconds_bucket", "up", "vector_x"}) {
		t.Errorf("metricsIn = %v", got)
	}
}

func TestDashboardQueriesExistingMetrics(t *testing.T) {
	names := promNames(t)
	b, err := fs.ReadFile(dashboards.FS, "grafana/service-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		UID    string `json:"uid"`
		Panels []struct {
			Title      string `json:"title"`
			Type       string `json:"type"`
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatalf("dashboard JSON: %v", err)
	}
	if dash.UID == "" {
		t.Error("dashboard has no uid")
	}
	queries := 0
	for _, p := range dash.Panels {
		if p.Type == "row" {
			continue
		}
		if p.Datasource.UID != "${datasource}" {
			t.Errorf("panel %q does not use the datasource variable", p.Title)
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, tg := range p.Targets {
			queries++
			// Every metric in the query needs its own $job selector.
			if n, want := strings.Count(tg.Expr, `job=~"$job"`), len(metricsIn(tg.Expr)); n < want {
				t.Errorf("panel %q: %d of %d metrics filter by $job: %s", p.Title, n, want, tg.Expr)
			}
			for _, m := range metricsIn(tg.Expr) {
				if !names[m] {
					t.Errorf("panel %q queries %s, which no package emits", p.Title, m)
				}
			}
		}
	}
	if queries < 30 {
		t.Errorf("only %d queries", queries)
	}
}

func TestAlertRules(t *testing.T) {
	names := promNames(t)
	b, err := fs.ReadFile(dashboards.FS, "prometheus/alerts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		t.Fatalf("alerts YAML: %v", err)
	}
	runbooks := runbookSections(t)
	seen := map[string]bool{}
	for _, g := range file.Groups {
		for _, r := range g.Rules {
			switch {
			case r.Alert == "" || seen[r.Alert]:
				t.Errorf("group %s: missing or duplicate alert name %q", g.Name, r.Alert)
			case r.For == "":
				t.Errorf("%s: no for duration", r.Alert)
			case r.Labels["severity"] != "warning" && r.Labels["severity"] != "critical":
				t.Errorf("%s: severity %q", r.Alert, r.Labels["severity"])
			case r.Annotations["summary"] == "" || r.Annotations["description"] == "":
				t.Errorf("%s: summary and description are required", r.Alert)
			case !strings.Contains(r.Expr, "by (job"):
				t.Errorf("%s: does not group by job", r.Alert)
			}
			if want := runbookBase + strings.ToLower(r.Alert); r.Annotations["runbook_url"] != want {
				t.Errorf("%s: runbook_url %q, want %q", r.Alert, r.Annotations["runbook_url"], want)
			}
			if !runbooks[strings.ToLower(r.Alert)] {
				t.Errorf("%s: no section \"### %s\" in RUNBOOKS.md", r.Alert, r.Alert)
			}
			seen[r.Alert] = true
			for _, m := range metricsIn(r.Expr) {
				if !names[m] {
					t.Errorf("%s queries %s, which no package emits", r.Alert, m)
				}
			}
		}
	}
	if len(seen) < 15 {
		t.Errorf("only %d alerts", len(seen))
	}
	for a := range runbooks {
		if !seenLower(seen, a) {
			t.Errorf("RUNBOOKS.md has a section for %s, which is not an alert", a)
		}
	}
}

// runbookBase is where the rules' runbook_url annotations point; the
// anchor is the alert name in lower case, as GitHub renders headings.
const runbookBase = "https://github.com/Arif9878/common/blob/main/go/observability/dashboards/RUNBOOKS.md#"

// runbookSections returns the lower-cased "### Name" headings of
// RUNBOOKS.md.
func runbookSections(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("RUNBOOKS.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]bool{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if name, ok := strings.CutPrefix(line, "### "); ok {
			sections[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return sections
}

func seenLower(seen map[string]bool, lower string) bool {
	for a := range seen {
		if strings.ToLower(a) == lower {
			return true
		}
	}
	return false
}
