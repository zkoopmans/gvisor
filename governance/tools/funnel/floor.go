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
// it. Every check is objective and fixable by the author in minutes: the
// floor says what a reviewer would otherwise have to say first. Everything
// that takes judgement is a reviewer's call.

// HoldLabel marks a pull request held at the floor.
const HoldLabel = "status: needs author"

// commentMarker identifies the floor's comment so it can be found, updated
// and removed. There is never more than one.
const commentMarker = "<!-- gvisor-floor -->"

// baseBranch is the only branch pull requests may target: `go` is generated
// from it, and release branches take cherry-picks, not pull requests.
const baseBranch = "master"

var (
	// assistedBy matches an `Assisted-by:` line with a value, anywhere in a
	// description or commit message. `Assisted-by: none` counts.
	assistedBy = regexp.MustCompile(`(?im)^assisted-by:[ \t]*\S`)

	// boilerplate matches a line that carries no description: a heading, a
	// trailer (`Assisted-by:`, `Signed-off-by:`, ...), or a bare issue
	// reference such as `Fixes #123`.
	boilerplate = regexp.MustCompile(`(?i)^[ \t]*(#.*|[a-z-]+-by:.*|(fixes|closes|updates|resolves)[ \t]+#?\d+[ \t]*)$`)

	// notReady matches a title that says the pull request is not ready:
	// `WIP`, `DNM`, `do not merge` anywhere, or a `[draft]`/`draft:` prefix.
	notReady = regexp.MustCompile(`(?i)\b(wip|dnm|do not (merge|review|submit))\b|^[ \t]*\[?draft\]?[ \t]*[:\-\]]`)
)

// Request is the part of a pull request the floor looks at.
type Request struct {
	Title        string
	Body         string
	BaseBranch   string
	ChangedFiles int
	// Mergeable is nil while GitHub has not yet computed it, which is usual
	// right after a push; the floor then assumes the best.
	Mergeable *bool
	Commits   []Commit
}

// Commit is the part of a commit the floor looks at.
type Commit struct {
	Message string
	Parents int
}

// Floor is the result of checking a pull request against the floor.
type Floor struct {
	// Missing lists what the pull request lacks, in the words shown to the
	// author. Empty means the pull request clears the floor.
	Missing []string
}

// Clear reports whether the pull request clears the floor.
func (f Floor) Clear() bool { return len(f.Missing) == 0 }

// CheckFloor checks a pull request against the floor. The shape of the pull
// request comes first, then what it says: a rebase changes the commits, so
// an author fixing the list top to bottom does not redo work.
func CheckFloor(r Request) Floor {
	var f Floor
	need := func(format string, args ...any) {
		f.Missing = append(f.Missing, fmt.Sprintf(format, args...))
	}

	if r.BaseBranch != baseBranch {
		need("to target `%s` rather than `%s`", baseBranch, r.BaseBranch)
	}
	if notReady.MatchString(r.Title) {
		need("to be marked as a draft rather than say so in the title: a draft waits for you without CI or reviewers")
	}
	if r.ChangedFiles == 0 {
		need("a change: the pull request touches no files")
	}
	if n := countMerges(r.Commits); n > 0 {
		need("a rebase: it contains %d merge %s, and gVisor's history is linear", n, plural(n, "commit"))
	}
	if r.Mergeable != nil && !*r.Mergeable {
		need("a rebase: it conflicts with `%s`", baseBranch)
	}

	texts := []string{r.Body}
	for _, c := range r.Commits {
		texts = append(texts, c.Message)
	}
	if !slices.ContainsFunc(texts, assistedBy.MatchString) {
		need("an `Assisted-by:` line in the description or a commit message (`Assisted-by: none` if no AI tool was used)")
	}
	if !hasProse(r.Body) {
		need("a description in your own words: what the change does and why")
	}
	return f
}

// countMerges returns how many of the commits are merge commits.
func countMerges(commits []Commit) int {
	n := 0
	for _, c := range commits {
		if c.Parents > 1 {
			n++
		}
	}
	return n
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
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
