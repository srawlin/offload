// Package report turns raw test output into a small, agent-sized summary.
//
// Parsers work on the tools' default human output so the agent still sees the
// native format. Each one recognises go test, cargo test, vitest, jest,
// mocha or pytest output; anything else gets the generic fallback.
package report

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Failure struct {
	Test     string `json:"test"`
	Location string `json:"location,omitempty"`
	Message  string `json:"message,omitempty"`
	Excerpt  string `json:"excerpt,omitempty"`
}

type Summary struct {
	Framework string    `json:"framework"`
	Passed    int       `json:"passed"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	Failures  []Failure `json:"failures,omitempty"`
	// Packages/suites that failed to build or run at all.
	Errors []string `json:"errors,omitempty"`
}

func (s Summary) Line() string {
	parts := []string{}
	if s.Failed > 0 || len(s.Failures) > 0 {
		n := s.Failed
		if n < len(s.Failures) {
			n = len(s.Failures)
		}
		parts = append(parts, fmt.Sprintf("%d failed", n))
	}
	if len(s.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d build/run errors", len(s.Errors)))
	}
	if s.Passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", s.Passed))
	}
	if s.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.Skipped))
	}
	if len(parts) == 0 {
		return "no test results recognised"
	}
	return strings.Join(parts, ", ")
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Parse picks the first parser that recognises the output.
func Parse(out string) Summary {
	out = ansi.ReplaceAllString(out, "")
	lines := strings.Split(out, "\n")
	for _, p := range []func([]string) (Summary, bool){parseCargo, parseGo, parseVitest, parseJest, parsePytest, parseMocha} {
		if s, ok := p(lines); ok {
			return s
		}
	}
	return Summary{Framework: "unknown"}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// excerpt returns up to n non-blank lines starting at i.
func excerpt(lines []string, i, n int, stop func(string) bool) string {
	var b []string
	for j := i; j < len(lines) && len(b) < n; j++ {
		if j > i && stop != nil && stop(lines[j]) {
			break
		}
		if strings.TrimSpace(lines[j]) != "" {
			b = append(b, strings.TrimRight(lines[j], " \r"))
		}
	}
	return strings.Join(b, "\n")
}

var (
	goFailTest = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	goPassTest = regexp.MustCompile(`^\s*--- PASS: `)
	goSkipTest = regexp.MustCompile(`^\s*--- SKIP: `)
	goOkPkg    = regexp.MustCompile(`^ok\s+(\S+)`)
	goFailPkg  = regexp.MustCompile(`^FAIL\s+(\S+)`)
	goLoc      = regexp.MustCompile(`^\s+(\S+\.go:\d+): (.*)`)
	goBuildErr = regexp.MustCompile(`^FAIL\s+(\S+) \[(build failed|setup failed)\]`)
)

func parseGo(lines []string) (Summary, bool) {
	s := Summary{Framework: "go"}
	seen := false
	for i, l := range lines {
		switch {
		case goBuildErr.MatchString(l):
			s.Errors = append(s.Errors, goBuildErr.FindStringSubmatch(l)[1]+": build failed")
			seen = true
		case goFailTest.MatchString(l):
			f := Failure{Test: goFailTest.FindStringSubmatch(l)[1]}
			// go test prints the failing assertions just after the --- FAIL line.
			if i+1 < len(lines) {
				if m := goLoc.FindStringSubmatch(lines[i+1]); m != nil {
					f.Location, f.Message = m[1], m[2]
				}
			}
			f.Excerpt = excerpt(lines, i, 8, func(x string) bool {
				return strings.HasPrefix(strings.TrimSpace(x), "--- ") || strings.HasPrefix(x, "FAIL") || strings.HasPrefix(x, "=== RUN")
			})
			s.Failures = append(s.Failures, f)
			s.Failed++
			seen = true
		case goPassTest.MatchString(l):
			s.Passed++
		case goSkipTest.MatchString(l):
			s.Skipped++
		case goOkPkg.MatchString(l):
			seen = true
			if !strings.Contains(l, "[no test files]") {
				s.Passed++ // a passing package counts once when not in -v mode
			}
		case goFailPkg.MatchString(l) && !goBuildErr.MatchString(l):
			seen = true
		}
	}
	return s, seen
}

var (
	cargoTest   = regexp.MustCompile(`^test (\S+) \.\.\. (ok|FAILED|ignored)`)
	cargoResult = regexp.MustCompile(`^test result: (ok|FAILED)\. (\d+) passed; (\d+) failed; (\d+) ignored`)
	cargoPanic  = regexp.MustCompile(`^---- (\S+) stdout ----`)
	cargoErr    = regexp.MustCompile(`^error(\[E\d+\])?: (.*)`)
	cargoLoc    = regexp.MustCompile(`^\s+--> (\S+:\d+:\d+)`)
)

func parseCargo(lines []string) (Summary, bool) {
	s := Summary{Framework: "cargo"}
	seen := false
	for i, l := range lines {
		if m := cargoResult.FindStringSubmatch(l); m != nil {
			seen = true
			s.Passed += atoi(m[2])
			s.Failed += atoi(m[3])
			s.Skipped += atoi(m[4])
		} else if m := cargoPanic.FindStringSubmatch(l); m != nil {
			f := Failure{Test: m[1], Excerpt: excerpt(lines, i+1, 8, func(x string) bool {
				return strings.HasPrefix(x, "---- ") || strings.HasPrefix(x, "failures:")
			})}
			for _, x := range strings.Split(f.Excerpt, "\n") {
				if strings.Contains(x, "panicked at") {
					f.Location = strings.TrimSuffix(strings.TrimSpace(x[strings.Index(x, "panicked at")+len("panicked at"):]), ":")
				} else if f.Message == "" && f.Location != "" {
					f.Message = strings.TrimSpace(x)
				}
			}
			s.Failures = append(s.Failures, f)
		} else if m := cargoErr.FindStringSubmatch(l); m != nil && !strings.HasPrefix(m[2], "test failed") && !strings.HasPrefix(m[2], "could not compile") {
			e := m[2]
			if i+1 < len(lines) {
				if lm := cargoLoc.FindStringSubmatch(lines[i+1]); lm != nil {
					e = lm[1] + ": " + e
				}
			}
			s.Errors = append(s.Errors, e)
			seen = true
		} else if cargoTest.MatchString(l) {
			seen = true
		}
	}
	return s, seen
}

var (
	vitestTests = regexp.MustCompile(`^\s+Tests\s+(.*)\(\d+\)`)
	vitestFail  = regexp.MustCompile(`^\s*(?:FAIL|×|✗)\s+(.+?)(?:\s+\d+ms)?$`)
	countRe     = regexp.MustCompile(`(\d+) (failed|passed|skipped|todo)`)
)

func parseVitest(lines []string) (Summary, bool) {
	s := Summary{Framework: "vitest"}
	seen := false
	for i, l := range lines {
		if m := vitestTests.FindStringSubmatch(l); m != nil {
			seen = true
			for _, c := range countRe.FindAllStringSubmatch(m[1], -1) {
				switch c[2] {
				case "failed":
					s.Failed = atoi(c[1])
				case "passed":
					s.Passed = atoi(c[1])
				default:
					s.Skipped += atoi(c[1])
				}
			}
		} else if m := vitestFail.FindStringSubmatch(l); m != nil && strings.Contains(m[1], " > ") {
			s.Failures = appendUnique(s.Failures, Failure{Test: m[1], Excerpt: excerpt(lines, i+1, 6, func(x string) bool {
				return vitestFail.MatchString(x) || strings.HasPrefix(strings.TrimSpace(x), "⎯")
			})})
		}
	}
	return s, seen
}

var (
	jestTests = regexp.MustCompile(`^Tests:\s+(.*)\d+ total`)
	jestFail  = regexp.MustCompile(`^\s+● (.+ › .+)$`)
)

func parseJest(lines []string) (Summary, bool) {
	s := Summary{Framework: "jest"}
	seen := false
	for i, l := range lines {
		if m := jestTests.FindStringSubmatch(l); m != nil {
			seen = true
			for _, c := range countRe.FindAllStringSubmatch(m[1], -1) {
				switch c[2] {
				case "failed":
					s.Failed = atoi(c[1])
				case "passed":
					s.Passed = atoi(c[1])
				default:
					s.Skipped += atoi(c[1])
				}
			}
		} else if m := jestFail.FindStringSubmatch(l); m != nil {
			s.Failures = appendUnique(s.Failures, Failure{Test: m[1], Excerpt: excerpt(lines, i+1, 8, func(x string) bool { return jestFail.MatchString(x) })})
		}
	}
	return s, seen
}

var (
	pytestSummary = regexp.MustCompile(`^=+ (.*\d+ (passed|failed|error).*) in [\d.]+s`)
	pytestFailed  = regexp.MustCompile(`^(FAILED|ERROR) (\S+)(?: - (.*))?`)
)

func parsePytest(lines []string) (Summary, bool) {
	s := Summary{Framework: "pytest"}
	seen := false
	for _, l := range lines {
		if m := pytestSummary.FindStringSubmatch(l); m != nil {
			seen = true
			for _, c := range regexp.MustCompile(`(\d+) (failed|passed|skipped|error|errors|xfailed)`).FindAllStringSubmatch(m[1], -1) {
				switch c[2] {
				case "failed", "error", "errors":
					s.Failed += atoi(c[1])
				case "passed":
					s.Passed = atoi(c[1])
				default:
					s.Skipped += atoi(c[1])
				}
			}
		} else if m := pytestFailed.FindStringSubmatch(l); m != nil {
			s.Failures = append(s.Failures, Failure{Test: m[2], Message: m[3]})
		}
	}
	return s, seen
}

var (
	mochaPassing = regexp.MustCompile(`^\s+(\d+) passing`)
	mochaFailing = regexp.MustCompile(`^\s+(\d+) failing`)
	mochaPending = regexp.MustCompile(`^\s+(\d+) pending`)
	mochaFail    = regexp.MustCompile(`^\s+(\d+)\) (.+)$`)
)

func parseMocha(lines []string) (Summary, bool) {
	s := Summary{Framework: "mocha"}
	seen, inFailures := false, false
	for i, l := range lines {
		switch {
		case mochaPassing.MatchString(l):
			seen = true
			s.Passed = atoi(mochaPassing.FindStringSubmatch(l)[1])
		case mochaFailing.MatchString(l):
			s.Failed = atoi(mochaFailing.FindStringSubmatch(l)[1])
			inFailures = true
		case mochaPending.MatchString(l):
			s.Skipped = atoi(mochaPending.FindStringSubmatch(l)[1])
		case inFailures && mochaFail.MatchString(l):
			s.Failures = append(s.Failures, Failure{Test: mochaFail.FindStringSubmatch(l)[2], Excerpt: excerpt(lines, i+1, 6, mochaFail.MatchString)})
		}
	}
	return s, seen
}

func appendUnique(fs []Failure, f Failure) []Failure {
	for _, x := range fs {
		if x.Test == f.Test {
			return fs
		}
	}
	return append(fs, f)
}

// Trim caps the summary to roughly budget tokens (≈4 chars per token) by
// dropping excerpts, then failures, from the end.
func (s *Summary) Trim(budgetTokens int) {
	size := func() int {
		n := 200
		for _, f := range s.Failures {
			n += len(f.Test) + len(f.Location) + len(f.Message) + len(f.Excerpt) + 40
		}
		for _, e := range s.Errors {
			n += len(e)
		}
		return n / 4
	}
	for i := len(s.Failures) - 1; i >= 0 && size() > budgetTokens; i-- {
		s.Failures[i].Excerpt = ""
	}
	for len(s.Failures) > 1 && size() > budgetTokens {
		s.Failures = s.Failures[:len(s.Failures)-1]
	}
	if len(s.Errors) > 20 {
		s.Errors = append(s.Errors[:20], fmt.Sprintf("… %d more", len(s.Errors)-20))
	}
}
