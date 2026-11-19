// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"strings"
	"testing"
)

func TestCheckFloor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		commits       []string
		wantAssisted  bool // Assisted-by: satisfied
		wantDescribed bool // description satisfied
	}{
		{"empty body", "", nil, false, false},
		{"prose, no trailer", "Fix the thing because reasons.\n\nFixes #123", nil, false, true},
		{"prose and none", "Fix the thing.\n\nAssisted-by: none", nil, true, true},
		{"only an issue reference", "Fixes #123", nil, false, false},
		{"only a heading", "## Summary\n\n", nil, false, false},
		{"only trailers", "Signed-off-by: A <a@b.c>\nAssisted-by: Gemini", nil, true, false},
		{"lowercase trailer", "Does stuff.\nassisted-by: Gemini (tests)", nil, true, true},
		{"trailer with no value", "Does stuff.\nAssisted-by:   ", nil, false, true},
		{"trailer in a commit only", "Does stuff.", []string{"fix: thing\n\nAssisted-by: Claude\nSigned-off-by: A <a@b.c>"}, true, true},
		{"trailer in a later commit", "Does stuff.", []string{"first", "second\n\nAssisted-by: none"}, true, true},
		{"trailer mid-line is not a trailer", "Does stuff. Assisted-by: none", nil, false, true},
		{"CRLF body", "Does stuff.\r\n\r\nAssisted-by: none\r\n", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := CheckFloor(tc.body, tc.commits)
			gotAssisted, gotDescribed := true, true
			for _, m := range f.Missing {
				switch {
				case strings.Contains(m, "Assisted-by"):
					gotAssisted = false
				case strings.Contains(m, "description"):
					gotDescribed = false
				default:
					t.Errorf("unexpected missing item %q", m)
				}
			}
			if gotAssisted != tc.wantAssisted {
				t.Errorf("Assisted-by satisfied = %v, want %v (missing: %q)", gotAssisted, tc.wantAssisted, f.Missing)
			}
			if gotDescribed != tc.wantDescribed {
				t.Errorf("description satisfied = %v, want %v (missing: %q)", gotDescribed, tc.wantDescribed, f.Missing)
			}
			if f.Clear() != (tc.wantAssisted && tc.wantDescribed) {
				t.Errorf("Clear() = %v, want %v", f.Clear(), tc.wantAssisted && tc.wantDescribed)
			}
		})
	}
}

func TestHoldComment(t *testing.T) {
	f := CheckFloor("", nil)
	c := holdComment(f)
	if !isHoldComment(c) {
		t.Errorf("holdComment is not recognised by isHoldComment:\n%s", c)
	}
	for _, m := range f.Missing {
		if !strings.Contains(c, m) {
			t.Errorf("comment does not mention %q", m)
		}
	}
	if isHoldComment("Looks good to me.") {
		t.Error("a human comment was taken for the floor's")
	}
}
