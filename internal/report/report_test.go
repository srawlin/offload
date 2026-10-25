package report

import "testing"

func TestParsers(t *testing.T) {
	cases := []struct {
		name, out, framework string
		passed, failed       int
		firstFailure         string
	}{
		{"go", "=== RUN   TestA\n--- FAIL: TestA (0.00s)\n    a_test.go:12: want 1 got 2\nFAIL\nFAIL\texample.com/a\t0.01s\nok  \texample.com/b\t0.02s\n", "go", 1, 1, "TestA"},
		{"go-build", "# example.com/a\na.go:3:1: syntax error\nFAIL\texample.com/a [build failed]\n", "go", 0, 0, ""},
		{"cargo", "running 2 tests\ntest a::ok ... ok\ntest a::bad ... FAILED\n\nfailures:\n\n---- a::bad stdout ----\nthread 'a::bad' panicked at src/lib.rs:9:5:\nassertion failed\n\nfailures:\n    a::bad\n\ntest result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out\n", "cargo", 1, 1, "a::bad"},
		{"vitest", " FAIL  src/a.test.ts > math > adds\nAssertionError: expected 3 to be 4\n\n Test Files  1 failed | 3 passed (4)\n      Tests  1 failed | 41 passed (42)\n", "vitest", 41, 1, "src/a.test.ts > math > adds"},
		{"jest", "  ● math › adds\n\n    expect(received).toBe(expected)\n\nTests:       1 failed, 9 passed, 10 total\n", "jest", 9, 1, "math › adds"},
		{"pytest", "FAILED tests/test_a.py::test_x - AssertionError: boom\n===== 1 failed, 5 passed in 0.12s =====\n", "pytest", 5, 1, "tests/test_a.py::test_x"},
		{"mocha", "  12 passing (40ms)\n  1 failing\n\n  1) app\n       GET /:\n     Error: expected 200\n", "mocha", 12, 1, "app"},
	}
	for _, c := range cases {
		s := Parse(c.out)
		if s.Framework != c.framework || s.Passed != c.passed || s.Failed != c.failed {
			t.Errorf("%s: got %+v", c.name, s)
		}
		if c.firstFailure != "" && (len(s.Failures) == 0 || s.Failures[0].Test != c.firstFailure) {
			t.Errorf("%s: failures %+v", c.name, s.Failures)
		}
	}
	if s := Parse("FAIL\texample.com/a [build failed]\n"); len(s.Errors) != 1 {
		t.Errorf("go build error not reported: %+v", s)
	}
}
