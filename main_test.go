package main

import "testing"

func TestBasename(t *testing.T) {
	testcases := map[string]struct {
		in       string
		expected string
	}{
		"simple": {
			in:       "example.org/cmd",
			expected: "cmd",
		},
		"version": {
			in:       "example.org/cmd/v2",
			expected: "cmd",
		},
	}

	for _, tc := range testcases {
		actual := basename(tc.in)
		if actual != tc.expected {
			t.Logf("unexpected result, input %q, expecting %q, actual %q", tc.in, tc.expected, actual)
			t.Fail()
		}
	}
}
