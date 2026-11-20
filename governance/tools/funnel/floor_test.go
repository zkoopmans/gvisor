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
	"slices"
	"strings"
	"testing"
)

// ok is a pull request that clears the floor; tests vary one thing at a time.
func ok() Request {
	yes := true
	return Request{
		Title:        "tcpip: fix the thing",
		Body:         "Fix the thing because reasons.\n\nAssisted-by: none",
		BaseBranch:   "master",
		ChangedFiles: 1,
		Mergeable:    &yes,
		Commits:      []Commit{{Message: "tcpip: fix the thing\n\nSigned-off-by: A <a@b.c>", Parents: 1}},
	}
}

// missing returns which checks a Floor failed, by a keyword in each message.
func missing(f Floor) []string {
	var got []string
	for _, m := range f.Missing {
		switch {
		case strings.Contains(m, "Assisted-by"):
			got = append(got, "assisted-by")
		case strings.Contains(m, "description"):
			got = append(got, "description")
		case strings.Contains(m, "target"):
			got = append(got, "base")
		case strings.Contains(m, "draft"):
			got = append(got, "wip")
		case strings.Contains(m, "touches no files"):
			got = append(got, "empty")
		case strings.Contains(m, "merge commit"):
			got = append(got, "merges")
		case strings.Contains(m, "conflicts"):
			got = append(got, "conflict")
		default:
			got = append(got, "?"+m)
		}
	}
	return got
}

func TestCheckFloor(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name string
		edit func(r *Request)
		want []string // failed checks, in the order the author sees them
	}{
		{"clears", func(r *Request) {}, nil},

		// Description and Assisted-by.
		{"empty body", func(r *Request) { r.Body = "" }, []string{"assisted-by", "description"}},
		{"prose, no trailer", func(r *Request) { r.Body = "Fix the thing because reasons.\n\nFixes #123" }, []string{"assisted-by"}},
		{"only an issue reference", func(r *Request) { r.Body = "Fixes #123" }, []string{"assisted-by", "description"}},
		{"only a heading", func(r *Request) { r.Body = "## Summary\n\n" }, []string{"assisted-by", "description"}},
		{"only trailers", func(r *Request) { r.Body = "Signed-off-by: A <a@b.c>\nAssisted-by: Gemini" }, []string{"description"}},
		{"lowercase trailer", func(r *Request) { r.Body = "Does stuff.\nassisted-by: Gemini (tests)" }, nil},
		{"trailer with no value", func(r *Request) { r.Body = "Does stuff.\nAssisted-by:   " }, []string{"assisted-by"}},
		{"trailer mid-line is not a trailer", func(r *Request) { r.Body = "Does stuff. Assisted-by: none" }, []string{"assisted-by"}},
		{"CRLF body", func(r *Request) { r.Body = "Does stuff.\r\n\r\nAssisted-by: none\r\n" }, nil},
		{"trailer in a commit only", func(r *Request) {
			r.Body = "Does stuff."
			r.Commits = []Commit{{Message: "fix: thing\n\nAssisted-by: Claude\nSigned-off-by: A <a@b.c>", Parents: 1}}
		}, nil},
		{"trailer in a later commit", func(r *Request) {
			r.Body = "Does stuff."
			r.Commits = []Commit{{Message: "first", Parents: 1}, {Message: "second\n\nAssisted-by: none", Parents: 1}}
		}, nil},

		// Shape.
		{"targets go", func(r *Request) { r.BaseBranch = "go" }, []string{"base"}},
		{"targets a release branch", func(r *Request) { r.BaseBranch = "release-20260727.0" }, []string{"base"}},
		{"WIP in title", func(r *Request) { r.Title = "WIP: tcpip: fix the thing" }, []string{"wip"}},
		{"[WIP] in title", func(r *Request) { r.Title = "[WIP] tcpip: fix the thing" }, []string{"wip"}},
		{"wip lowercase mid-title", func(r *Request) { r.Title = "tcpip: fix the thing (wip)" }, []string{"wip"}},
		{"do not merge", func(r *Request) { r.Title = "tcpip: fix the thing -- DO NOT MERGE" }, []string{"wip"}},
		{"DNM", func(r *Request) { r.Title = "DNM tcpip: fix the thing" }, []string{"wip"}},
		{"draft prefix", func(r *Request) { r.Title = "Draft: tcpip: fix the thing" }, []string{"wip"}},
		{"[draft] prefix", func(r *Request) { r.Title = "[draft] tcpip: fix the thing" }, []string{"wip"}},
		{"draft as a word is fine", func(r *Request) { r.Title = "tcpip: handle draft segments" }, nil},
		{"wipe is not wip", func(r *Request) { r.Title = "mm: wipe the page cache" }, nil},
		{"no files", func(r *Request) { r.ChangedFiles = 0 }, []string{"empty"}},
		{"one merge commit", func(r *Request) {
			r.Commits = append(r.Commits, Commit{Message: "Merge branch 'master' into fix", Parents: 2})
		}, []string{"merges"}},
		{"octopus merge counts once", func(r *Request) {
			r.Commits = append(r.Commits, Commit{Message: "Merge", Parents: 3})
		}, []string{"merges"}},
		{"root commit is not a merge", func(r *Request) { r.Commits[0].Parents = 0 }, nil},
		{"conflicts", func(r *Request) { r.Mergeable = &no }, []string{"conflict"}},
		{"mergeability unknown assumes the best", func(r *Request) { r.Mergeable = nil }, nil},

		// Order: shape before words, so fixing top to bottom is not redone.
		{"everything wrong, in order", func(r *Request) {
			*r = Request{Title: "WIP", BaseBranch: "go", Mergeable: &no, Commits: []Commit{{Message: "Merge", Parents: 2}}}
		}, []string{"base", "wip", "empty", "merges", "conflict", "assisted-by", "description"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ok()
			tc.edit(&r)
			f := CheckFloor(r)
			if got := missing(f); !slices.Equal(got, tc.want) {
				t.Errorf("failed checks = %q, want %q\nmessages: %q", got, tc.want, f.Missing)
			}
			if f.Clear() != (len(tc.want) == 0) {
				t.Errorf("Clear() = %v, want %v", f.Clear(), len(tc.want) == 0)
			}
		})
	}
}

func TestMergeCommitMessage(t *testing.T) {
	r := ok()
	r.Commits = append(r.Commits, Commit{Parents: 2})
	if f := CheckFloor(r); !strings.Contains(f.Missing[0], "1 merge commit,") {
		t.Errorf("singular: %q", f.Missing[0])
	}
	r.Commits = append(r.Commits, Commit{Parents: 2})
	if f := CheckFloor(r); !strings.Contains(f.Missing[0], "2 merge commits,") {
		t.Errorf("plural: %q", f.Missing[0])
	}
}

func TestHoldComment(t *testing.T) {
	f := CheckFloor(Request{})
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
