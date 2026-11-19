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
	"math/rand"
	"slices"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// Reviewer assignment: the approvers of the areas covering the changed files,
// filtered to active maintainers, with a random roster fallback. Up to two
// reviewers: one area specialist when the area has one, the rest from the
// roster so that knowledge spreads.

// maxReviewers is how many reviewers a pull request is assigned.
const maxReviewers = 2

// maintainer is the subset of governance/maintainers.yaml the assigner reads.
type maintainer struct {
	GitHub string   `yaml:"github"`
	Status string   `yaml:"status"`
	Areas  []string `yaml:"areas"`
}

// area is the subset of governance/areas.yaml the assigner reads.
type area struct {
	Name  string   `yaml:"name"`
	Paths []string `yaml:"paths"`
}

// Roster is who can review what, as loaded from the governance files.
type Roster struct {
	maintainers []maintainer
	areas       []area
}

// LoadRoster parses governance/maintainers.yaml and governance/areas.yaml.
func LoadRoster(maintainersYAML, areasYAML []byte) (*Roster, error) {
	var m struct {
		Maintainers []maintainer `yaml:"maintainers"`
	}
	if err := yaml.Unmarshal(maintainersYAML, &m); err != nil {
		return nil, fmt.Errorf("maintainers.yaml: %w", err)
	}
	var a struct {
		Areas []area `yaml:"areas"`
	}
	if err := yaml.Unmarshal(areasYAML, &a); err != nil {
		return nil, fmt.Errorf("areas.yaml: %w", err)
	}
	return &Roster{maintainers: m.Maintainers, areas: a.Areas}, nil
}

// Candidates returns who may review a pull request by author touching files:
// eligible is every active maintainer other than the author; specialists are
// the eligible maintainers of the areas covering the files. Files are
// repository-root-relative with a leading slash.
func (r *Roster) Candidates(author string, files []string) (eligible, specialists []string) {
	byArea := map[string][]string{}
	for _, m := range r.maintainers {
		if m.Status != "ACTIVE" || m.GitHub == "" || strings.EqualFold(m.GitHub, author) {
			continue
		}
		eligible = append(eligible, m.GitHub)
		for _, a := range m.Areas {
			byArea[a] = append(byArea[a], m.GitHub)
		}
	}
	for _, name := range MatchAreas(r.areas, files) {
		for _, login := range byArea[name] {
			if !slices.Contains(specialists, login) {
				specialists = append(specialists, login)
			}
		}
	}
	return eligible, specialists
}

// MatchAreas returns the names of the areas covering any of the files, in
// areas.yaml order.
func MatchAreas(areas []area, files []string) []string {
	var matched []string
	for _, a := range areas {
		if slices.ContainsFunc(a.Paths, func(p string) bool {
			return slices.ContainsFunc(files, func(f string) bool { return pathMatches(p, f) })
		}) {
			matched = append(matched, a.Name)
		}
	}
	return matched
}

// pathMatches reports whether file f is path p or lies beneath it.
func pathMatches(p, f string) bool {
	return f == p || strings.HasPrefix(f, p+"/")
}

// Pick chooses the reviewers: one specialist at random when there is one,
// then the remaining seats from the rest of the eligible roster at random.
func Pick(eligible, specialists []string, rng *rand.Rand) []string {
	var picked []string
	if len(specialists) > 0 {
		picked = append(picked, shuffled(specialists, rng)[0])
	}
	var rest []string
	for _, login := range eligible {
		if !slices.ContainsFunc(picked, func(p string) bool { return strings.EqualFold(p, login) }) {
			rest = append(rest, login)
		}
	}
	for _, login := range shuffled(rest, rng) {
		if len(picked) >= maxReviewers {
			break
		}
		picked = append(picked, login)
	}
	return picked
}

// shuffled returns a shuffled copy of s.
func shuffled(s []string, rng *rand.Rand) []string {
	out := slices.Clone(s)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
