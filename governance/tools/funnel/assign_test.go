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
	"math/rand"
	"slices"
	"strings"
	"testing"
)

const testMaintainers = `
maintainers:
  - github: alice
    status: ACTIVE
    areas: [networking, sentry]
  - github: bob
    status: ACTIVE
    areas: [networking]
  - github: carol
    status: ACTIVE
    areas: [platforms]
  - github: dave
    status: ACTIVE
  - github: erin
    status: EMERITUS
    areas: [networking, sentry, platforms]
  - github: frank
    status: HIATUS
    areas: [sentry]
`

const testAreas = `
areas:
  - name: networking
    paths: [/pkg/tcpip]
  - name: platforms
    paths: [/pkg/sentry/platform]
  - name: sentry
    paths: [/pkg/sentry]
`

func testRoster(t *testing.T) *Roster {
	t.Helper()
	r, err := LoadRoster([]byte(testMaintainers), []byte(testAreas))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPathMatches(t *testing.T) {
	for _, tc := range []struct {
		p, f string
		want bool
	}{
		{"/pkg/sentry", "/pkg/sentry", true},
		{"/pkg/sentry", "/pkg/sentry/kernel/task.go", true},
		{"/pkg/sentry", "/pkg/sentryfoo/x.go", false},
		{"/pkg/sentry", "/pkg/sentr", false},
		{"/pkg/sentry", "/runsc/pkg/sentry/x.go", false},
		{"/pkg/tcpip", "/pkg/tcpip/stack/stack.go", true},
	} {
		if got := pathMatches(tc.p, tc.f); got != tc.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tc.p, tc.f, got, tc.want)
		}
	}
}

func TestMatchAreas(t *testing.T) {
	r := testRoster(t)
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"no files", nil, nil},
		{"unowned file", []string{"/README.md"}, nil},
		{"one area", []string{"/pkg/tcpip/stack/stack.go"}, []string{"networking"}},
		{"nested areas both match (today's semantics)", []string{"/pkg/sentry/platform/kvm/kvm.go"}, []string{"platforms", "sentry"}},
		{"two files two areas, yaml order", []string{"/pkg/sentry/kernel/task.go", "/pkg/tcpip/x.go"}, []string{"networking", "sentry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchAreas(r.areas, tc.files); !slices.Equal(got, tc.want) {
				t.Errorf("MatchAreas(%q) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	r := testRoster(t)
	for _, tc := range []struct {
		name            string
		author          string
		files           []string
		wantEligible    []string
		wantSpecialists []string
	}{
		{"outside author", "mallory", []string{"/pkg/tcpip/x.go"}, []string{"alice", "bob", "carol", "dave"}, []string{"alice", "bob"}},
		{"author excluded, case-insensitively", "Alice", []string{"/pkg/tcpip/x.go"}, []string{"bob", "carol", "dave"}, []string{"bob"}},
		{"inactive never eligible", "mallory", []string{"/pkg/sentry/x.go"}, []string{"alice", "bob", "carol", "dave"}, []string{"alice"}},
		{"specialists deduplicated across areas", "mallory", []string{"/pkg/sentry/platform/x.go", "/pkg/sentry/y.go"}, []string{"alice", "bob", "carol", "dave"}, []string{"carol", "alice"}},
		{"no files", "mallory", nil, []string{"alice", "bob", "carol", "dave"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eligible, specialists := r.Candidates(tc.author, tc.files)
			if !slices.Equal(eligible, tc.wantEligible) {
				t.Errorf("eligible = %q, want %q", eligible, tc.wantEligible)
			}
			if !slices.Equal(specialists, tc.wantSpecialists) {
				t.Errorf("specialists = %q, want %q", specialists, tc.wantSpecialists)
			}
		})
	}
}

func TestPick(t *testing.T) {
	eligible := []string{"alice", "bob", "carol", "dave"}
	for _, tc := range []struct {
		name        string
		eligible    []string
		specialists []string
		wantN       int
		wantFirstIn []string // the first pick must come from here, if set
	}{
		{"specialist first, then roster", eligible, []string{"alice", "bob"}, 2, []string{"alice", "bob"}},
		{"no specialists: two from roster", eligible, nil, 2, nil},
		{"one eligible: one reviewer", []string{"alice"}, nil, 1, nil},
		{"one eligible who is the specialist: no duplicate", []string{"alice"}, []string{"alice"}, 1, []string{"alice"}},
		{"nobody", nil, nil, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Many seeds: the properties must hold whatever the shuffle does.
			for seed := int64(0); seed < 50; seed++ {
				got := Pick(tc.eligible, tc.specialists, rand.New(rand.NewSource(seed)))
				if len(got) != tc.wantN {
					t.Fatalf("seed %d: picked %q, want %d reviewers", seed, got, tc.wantN)
				}
				if tc.wantFirstIn != nil && !slices.Contains(tc.wantFirstIn, got[0]) {
					t.Fatalf("seed %d: first pick %q not a specialist %q", seed, got[0], tc.wantFirstIn)
				}
				for i, a := range got {
					for _, b := range got[i+1:] {
						if strings.EqualFold(a, b) {
							t.Fatalf("seed %d: %q picked twice", seed, a)
						}
					}
					if !slices.Contains(tc.eligible, a) {
						t.Fatalf("seed %d: %q is not eligible", seed, a)
					}
				}
			}
		})
	}
}

func TestPickIsRandom(t *testing.T) {
	// Over many draws every roster member should get the second seat
	// sometimes; a bias here is what "knowledge sharing" relies on not having.
	eligible := []string{"alice", "bob", "carol", "dave"}
	seen := map[string]bool{}
	for seed := int64(0); seed < 200; seed++ {
		got := Pick(eligible, []string{"alice"}, rand.New(rand.NewSource(seed)))
		seen[got[1]] = true
	}
	for _, login := range []string{"bob", "carol", "dave"} {
		if !seen[login] {
			t.Errorf("%s never drawn for the second seat in 200 draws", login)
		}
	}
	if seen["alice"] {
		t.Error("the specialist was drawn twice")
	}
}
