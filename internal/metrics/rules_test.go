package metrics_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tokayops/tokayops/internal/metrics"
	// The delivery package initialises the zero series of its vectors at
	// registration, which is what the rules on them stand on.
	_ "github.com/tokayops/tokayops/internal/outbound"
	"gopkg.in/yaml.v3"
)

// The rules file names series; the registry exports series. A rule on a
// series nobody exports is a rule that never fires and never says so - which
// is how the rules that lived in prose alerted on metrics removed two sprints
// earlier. This test reads every expression in deploy/prometheus and checks
// each name it uses against what the registry describes and what the
// business collector reads from the database.

type rulesFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string `yaml:"alert"`
			Record string `yaml:"record"`
			Expr   string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

var (
	identifier    = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*`)
	labelMatchers = regexp.MustCompile(`\{[^}]*\}`)
	rangeSelector = regexp.MustCompile(`\[[^\]]*\]`)
	// The label lists after by, without, on and ignoring name labels, not
	// series.
	labelLists = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
)

// promqlKeywords are the words of the language that look like identifiers
// and are not series.
var promqlKeywords = map[string]bool{
	"and": true, "or": true, "unless": true, "bool": true, "offset": true, "by": true,
	"without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"inf": true, "nan": true, "Inf": true, "NaN": true,
}

// seriesNamed is every series name an expression selects. Functions are told
// apart by the parenthesis that follows them; label names are removed with
// the matchers and lists they live in; the rest of the identifiers are the
// series. The language's aggregation and function names never stand alone
// as operands, so what remains is what the rule reads.
func seriesNamed(expr string) []string {
	cleaned := labelMatchers.ReplaceAllString(expr, "")
	cleaned = rangeSelector.ReplaceAllString(cleaned, "")
	cleaned = labelLists.ReplaceAllString(cleaned, "")
	var names []string
	for _, m := range identifier.FindAllStringIndex(cleaned, -1) {
		word := cleaned[m[0]:m[1]]
		if promqlKeywords[word] {
			continue
		}
		// A function call: the identifier is followed by "(".
		rest := strings.TrimLeft(cleaned[m[1]:], " \t\n")
		if strings.HasPrefix(rest, "(") {
			continue
		}
		// A duration or a number is not an identifier by the regexp, but a
		// unit suffix after digits is: "5m" leaves "m". Skip identifiers
		// preceded by a digit.
		if m[0] > 0 && cleaned[m[0]-1] >= '0' && cleaned[m[0]-1] <= '9' {
			continue
		}
		names = append(names, word)
	}
	return names
}

func TestSeriesNamedReadsExpressions(t *testing.T) {
	for expr, want := range map[string][]string{
		`outbound_queue_lateness_seconds{family="notification"} > 60`:                                                                {"outbound_queue_lateness_seconds"},
		`histogram_quantile(0.99, sum by (le) (rate(outbound_admission_latency_seconds_bucket{family="notification"}[1h]))) > 10`:    {"outbound_admission_latency_seconds_bucket"},
		`max(outbound_retention_window_days) > 0 and (time() - max(outbound_retention_last_success_timestamp_seconds)) > 7200`:       {"outbound_retention_window_days", "outbound_retention_last_success_timestamp_seconds"},
		`max(a_total) > 0 unless max(b_seconds)`:                                                                                     {"a_total", "b_seconds"},
		`absent(outbound_queue_lateness_seconds{family="x"})`:                                                                        {"outbound_queue_lateness_seconds"},
		`sum by (family, provider) (rate(x_total{outcome="ambiguous"}[15m])) / sum by (family, provider) (rate(x_total[15m])) > 0.1`: {"x_total", "x_total"},
		`rate(outbound_worker_ticks_total[5m]) == 0`:                                                                                 {"outbound_worker_ticks_total"},
		`teams_without_oncall > 0`: {"teams_without_oncall"},
	} {
		if got := seriesNamed(expr); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s\n  reads %v\n  want  %v", expr, got, want)
		}
	}
}

// exportedSeries is every family name the process exports: what the registry
// describes (vectors with no series yet included), what it has gathered, and
// what the business collector reads from the database on each scrape.
func exportedSeries(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, name := range metrics.Described() {
		names[name] = true
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		names[family.GetName()] = true
	}
	// The collector describes its series without a store: the descriptions
	// are the collector's own, the values are the database's.
	for _, name := range metrics.DescribedBy(&metrics.BusinessCollector{}) {
		names[name] = true
	}
	return names
}

// TestEveryRuleNamesAMetricTheRegistryExports: each series name in each
// expression of the rules file is one the process exports, one of a
// histogram's own families, or one a recording rule of the same file makes.
func TestEveryRuleNamesAMetricTheRegistryExports(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "prometheus", "tokayops.rules.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var file rulesFile
	if err := yaml.Unmarshal(body, &file); err != nil {
		t.Fatalf("parse the rules file: %v", err)
	}
	exported := exportedSeries(t)
	recorded := map[string]bool{}
	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			if rule.Record != "" {
				recorded[rule.Record] = true
			}
		}
	}
	known := func(name string) bool {
		if exported[name] || recorded[name] {
			return true
		}
		// A histogram exports _bucket, _sum and _count under its own name.
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(name, suffix) && exported[strings.TrimSuffix(name, suffix)] {
				return true
			}
		}
		return false
	}

	rules := 0
	var unknown []string
	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			rules++
			for _, name := range seriesNamed(rule.Expr) {
				if !known(name) {
					label := rule.Alert
					if label == "" {
						label = rule.Record
					}
					unknown = append(unknown, label+" reads "+name)
				}
			}
		}
	}
	if rules < 20 {
		t.Fatalf("%d rules read; the file has more than that", rules)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("rules on series nothing exports:\n  %s", strings.Join(unknown, "\n  "))
	}
}
