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
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The floor is what a pull request must have before anyone is asked to read
// it. It is deliberately small: a description in the author's words, and a
// line saying how AI tools were used. Everything else is a reviewer's call.

// HoldLabel marks a pull request held at the floor.
const HoldLabel = "status: needs author"

// commentMarker identifies the floor's comment so it can be found, updated
// and removed. There is never more than one.
const commentMarker = "<!-- gvisor-floor -->"

var (
	// assistedBy matches an `Assisted-by:` line with a value, anywhere in a
	// description or commit message. `Assisted-by: none` counts.
	assistedBy = regexp.MustCompile(`(?im)^assisted-by:[ \t]*\S`)

	// boilerplate matches a line that carries no description: a heading, a
	// trailer (`Assisted-by:`, `Signed-off-by:`, ...), or a bare issue
	// reference such as `Fixes #123`.
	boilerplate = regexp.MustCompile(`(?i)^[ \t]*(#.*|[a-z-]+-by:.*|(fixes|closes|updates|resolves)[ \t]+#?\d+[ \t]*)$`)
)

// Floor is the result of checking a pull request against the floor.
type Floor struct {
	// Missing lists what the pull request lacks, in the words shown to the
	// author. Empty means the pull request clears the floor.
	Missing []string
}

// Clear reports whether the pull request clears the floor.
func (f Floor) Clear() bool { return len(f.Missing) == 0 }

// CheckFloor checks a pull request's description and commit messages.
func CheckFloor(body string, commits []string) Floor {
	var f Floor
	texts := append([]string{body}, commits...)
	if !slices.ContainsFunc(texts, assistedBy.MatchString) {
		f.Missing = append(f.Missing, "an `Assisted-by:` line in the description or a commit message (`Assisted-by: none` if no AI tool was used)")
	}
	if !hasProse(body) {
		f.Missing = append(f.Missing, "a description in your own words: what the change does and why")
	}
	return f
}

// hasProse reports whether body says anything: headings, trailers and issue
// references alone do not count.
func hasProse(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || boilerplate.MatchString(line) {
			continue
		}
		return true
	}
	return false
}

// holdComment is the one comment the floor leaves on a held pull request.
func holdComment(f Floor) string {
	var b strings.Builder
	b.WriteString(commentMarker)
	b.WriteString("\nThanks for the pull request. Before it is routed to a reviewer it needs:\n\n")
	for _, m := range f.Missing {
		fmt.Fprintf(&b, "- %s\n", m)
	}
	b.WriteString("\nNo reviewer has been asked yet, and none will be until this passes. ")
	b.WriteString("This comment updates itself when the pull request is edited. ")
	b.WriteString("See [CONTRIBUTING.md](https://github.com/gvisor/gvisor/blob/master/CONTRIBUTING.md#ai-tool-use).")
	return b.String()
}

// isHoldComment reports whether body is the floor's comment.
func isHoldComment(body string) bool { return strings.Contains(body, commentMarker) }
