package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/obsregistry"
)

// repoRoot is this package's path back to the repository root.
const repoRoot = "../../../.."

// ruleFiles are every Prometheus rule file in the repo: the standalone
// reference copies and the chart's Helm-rendered twin.
var ruleFiles = []string{
	"deploy/monitoring/app-alerts.yml",
	"deploy/monitoring/slo-rules.yml",
	"deploy/helm/templates/prometheusrule.yaml",
}

// monitoringFiles are every file that queries metrics (rules, HPA and
// prometheus-adapter) and must not name a removed metric, even in a comment.
var monitoringFiles = append([]string{
	"deploy/helm/templates/hpa.yaml",
	"deploy/monitoring/prometheus-adapter-rule.yaml",
	"deploy/helm/templates/servicemonitor.yaml",
}, ruleFiles...)

var metricToken = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_:]*`)

// lookupMetric resolves a PromQL token to its Platform Observability
// Registry entry, mapping histogram series (_bucket/_count/_sum) to their
// family.
func lookupMetric(token string) (*obsregistry.Metric, bool) {
	reg := obsregistry.Default()
	if m, ok := reg.Lookup(token); ok {
		return m, true
	}
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if base, ok := strings.CutSuffix(token, suffix); ok {
			if m, ok := reg.Lookup(base); ok {
				return m, true
			}
		}
	}
	return nil, false
}

// helmTemplate matches Go-template actions in the chart's PrometheusRule so
// it parses as plain YAML: escaped "{{ "{{ $value }}" }}" annotations keep
// their inner text, {{ $job | quote }} becomes the default fullname, and
// directive-only lines are dropped.
var (
	helmEscaped   = regexp.MustCompile(`\{\{ "(\{\{[^"]*\}\})" \}\}`)
	helmJob       = regexp.MustCompile(`\{\{ \$job \| quote \}\}`)
	helmValue     = regexp.MustCompile(`\{\{ \.Values\.[a-zA-Z.]+ \}\}`)
	helmDirective = regexp.MustCompile(`^\s*\{\{-?.*\}\}\s*$`)
	helmComment   = regexp.MustCompile(`(?s)\{\{-? /\*.*?\*/ -?\}\}`)
)

func readRuleFile(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, rel))
	require.NoError(t, err)
	if !strings.HasPrefix(rel, "deploy/helm/") {
		return data
	}
	s := helmComment.ReplaceAllString(string(data), "")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if helmDirective.MatchString(line) {
			continue
		}
		line = helmEscaped.ReplaceAllString(line, "$1")
		line = helmJob.ReplaceAllString(line, `"iam-realm-provisioner"`)
		line = helmValue.ReplaceAllString(line, "2")
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

type promRule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type promRuleFile struct {
	Groups []struct {
		Name  string     `yaml:"name"`
		Rules []promRule `yaml:"rules"`
	} `yaml:"groups"`
	Spec struct {
		Groups []struct {
			Name  string     `yaml:"name"`
			Rules []promRule `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

func (f promRuleFile) rules() []promRule {
	var out []promRule
	for _, g := range f.Groups {
		out = append(out, g.Rules...)
	}
	for _, g := range f.Spec.Groups {
		out = append(out, g.Rules...)
	}
	return out
}

// TestMonitoring_NoRemovedMetrics fails on any removed (pre-standard)
// metric name in a rule, HPA or adapter file — even in a comment — naming
// its registry replacement. It is the in-repo equivalent of
// `metricslint refs` for these files.
func TestMonitoring_NoRemovedMetrics(t *testing.T) {
	for _, rel := range monitoringFiles {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			for _, tok := range metricToken.FindAllString(line, -1) {
				if m, ok := lookupMetric(tok); ok && m.Status == obsregistry.StatusRemoved {
					t.Errorf("%s:%d: %s was removed from the platform libraries; use %s", rel, i+1, tok, strings.Join(m.Replacements, " or "))
				}
			}
		}
	}
}

// TestMonitoring_ProposedMetricsAreLabelled enforces the routing contract
// for metrics still awaiting registry ratification: an alert that depends
// on a "proposed" metric (directly or through a recording rule in the same
// file) must carry metric_status: proposed, and an alert that does not
// must not. When the registry approves a metric this test fails until the
// label is dropped, so the label always tells the truth.
func TestMonitoring_ProposedMetricsAreLabelled(t *testing.T) {
	alerts := 0
	for _, rel := range ruleFiles {
		var f promRuleFile
		require.NoError(t, yaml.Unmarshal(readRuleFile(t, rel), &f), rel)
		rules := f.rules()
		require.NotEmpty(t, rules, rel)

		proposedDeps := func(expr string, records map[string][]string) []string {
			set := map[string]bool{}
			for _, tok := range metricToken.FindAllString(expr, -1) {
				if m, ok := lookupMetric(tok); ok && m.Status == obsregistry.StatusProposed {
					set[m.Name] = true
				}
				for _, dep := range records[tok] {
					set[dep] = true
				}
			}
			out := make([]string, 0, len(set))
			for k := range set {
				out = append(out, k)
			}
			sort.Strings(out)
			return out
		}
		records := map[string][]string{}
		for _, r := range rules {
			if r.Record != "" {
				records[r.Record] = proposedDeps(r.Expr, nil)
			}
		}
		for _, r := range rules {
			if r.Alert == "" {
				continue
			}
			alerts++
			deps := proposedDeps(r.Expr, records)
			if len(deps) > 0 {
				assert.Equal(t, "proposed", r.Labels["metric_status"],
					"%s: alert %s reads proposed metric(s) %v and must be labeled metric_status: proposed", rel, r.Alert, deps)
			} else {
				assert.Empty(t, r.Labels["metric_status"],
					"%s: alert %s reads no proposed metric; drop its metric_status label", rel, r.Alert)
			}
		}
	}
	assert.Greater(t, alerts, 0)

	hpa, err := os.ReadFile(filepath.Join(repoRoot, "deploy/helm/templates/hpa.yaml"))
	require.NoError(t, err)
	if strings.Contains(string(hpa), "platform_http_requests_per_second") {
		assert.Contains(t, string(hpa), "observability.platform/metric-status: proposed",
			"the HPA scales on platform_http_requests_total (proposed) and must carry the metric-status annotation")
	}
}

// runbookDoc is the alert-runbook page every alert's `runbook` annotation
// points into (docs/observability/runbooks.md#<alert-name-lowercase>).
const runbookDoc = "docs/observability/runbooks.md"

var runbookHeading = regexp.MustCompile(`(?m)^### (\S+)\s*$`)

// TestMonitoring_EveryAlertHasARunbook pins the alert ↔ runbook contract:
// every alert carries `runbook: docs/observability/runbooks.md#<name>`
// (the lowercased alert name), every such anchor is a `###` heading in that
// page, and the page has no heading for an alert that no longer exists.
func TestMonitoring_EveryAlertHasARunbook(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot, runbookDoc))
	require.NoError(t, err)
	headings := map[string]bool{}
	for _, m := range runbookHeading.FindAllStringSubmatch(string(doc), -1) {
		headings[strings.ToLower(m[1])] = true
	}

	alerts := map[string]bool{}
	for _, rel := range ruleFiles {
		var f promRuleFile
		require.NoError(t, yaml.Unmarshal(readRuleFile(t, rel), &f), rel)
		for _, r := range f.rules() {
			if r.Alert == "" {
				continue
			}
			anchor := strings.ToLower(r.Alert)
			alerts[anchor] = true
			assert.Equal(t, runbookDoc+"#"+anchor, r.Annotations["runbook"], "%s: alert %s needs a runbook annotation", rel, r.Alert)
			assert.True(t, headings[anchor], "%s: alert %s has no `### %s` entry in %s", rel, r.Alert, r.Alert, runbookDoc)
		}
	}
	for h := range headings {
		assert.True(t, alerts[h], "%s has an entry for %q, which is not an alert in any rule file", runbookDoc, h)
	}
}
