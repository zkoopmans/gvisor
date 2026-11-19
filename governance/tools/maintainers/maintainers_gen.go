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

// Command maintainers_gen generates the contents of MAINTAINERS.md and
// CODEOWNERS.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

var (
	input     = flag.String("input", "", "path to maintainers.yaml")
	areasPath = flag.String("areas", "", "path to areas.yaml")
	format    = flag.String("format", "MAINTAINERS.md", `output format: "MAINTAINERS.md" or "CODEOWNERS"`)
	output    = flag.String("output", "", "path to write to; defaults to stdout")
)

// pastAffiliation is a past employer of a maintainer.
type pastAffiliation struct {
	Affiliation string `yaml:"affiliation"`
	Until       string `yaml:"until"`
}

// maintainer is a single entry of the maintainer roster.
type maintainer struct {
	Name             string            `yaml:"name"`
	GitHub           string            `yaml:"github"`
	GitHubID         uint64            `yaml:"github_id"`
	Affiliation      string            `yaml:"affiliation"`
	PastAffiliations []pastAffiliation `yaml:"past_affiliation"`
	Started          string            `yaml:"started"`
	Status           string            `yaml:"status"`
}

// roster is the schema of maintainers.yaml.
type roster struct {
	Maintainers []maintainer `yaml:"maintainers"`
}

// area is a single entry of areas.yaml.
type area struct {
	Name           string   `yaml:"name"`
	EnforcedReview *bool    `yaml:"enforced_review"`
	Owners         []string `yaml:"owners"`
	Approvers      []string `yaml:"approvers"`
	Paths          []string `yaml:"paths"`
}

// areaList is the schema of areas.yaml.
type areaList struct {
	Areas []area `yaml:"areas"`
}

// parseAreas parses and validates areas.yaml contents, returning a map of
// area name to the area.
func parseAreas(yamlData []byte) (map[string]area, error) {
	dec := yaml.NewDecoder(bytes.NewReader(yamlData))
	dec.KnownFields(true)
	var al areaList
	if err := dec.Decode(&al); err != nil {
		return nil, fmt.Errorf("cannot parse areas: %w", err)
	}
	byName := make(map[string]area, len(al.Areas))
	pathArea := make(map[string]string)
	var prev area
	for _, a := range al.Areas {
		if a.Name == "" || a.EnforcedReview == nil || len(a.Paths) == 0 {
			return nil, fmt.Errorf("area %+v: name, enforced_review and paths are required", a)
		}
		if a.Name <= prev.Name {
			return nil, fmt.Errorf("areas must be sorted by name and unique: %q comes after %q", a.Name, prev.Name)
		}
		prev = a
		if err := checkSortedUnique("approvers", a.Approvers); err != nil {
			return nil, fmt.Errorf("area %q: %w", a.Name, err)
		}
		if err := checkSortedUnique("owners", a.Owners); err != nil {
			return nil, fmt.Errorf("area %q: %w", a.Name, err)
		}
		for _, o := range a.Owners {
			if !slices.Contains(a.Approvers, o) {
				return nil, fmt.Errorf("area %q: owner %q is not an approver; owners must be a subset of approvers", a.Name, o)
			}
		}
		if *a.EnforcedReview {
			if len(a.Approvers) == 0 {
				return nil, fmt.Errorf("area %q: enforced_review requires at least one approver", a.Name)
			}
			if len(a.Owners) == 0 {
				return nil, fmt.Errorf("area %q: enforced_review requires at least one owner", a.Name)
			}
		}
		for _, p := range a.Paths {
			if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
				return nil, fmt.Errorf("area %q: path %q must start with a slash and not end with one", a.Name, p)
			}
			if strings.Contains(p, "**") {
				return nil, fmt.Errorf("area %q: path %q: only single-segment wildcards (*) are supported", a.Name, p)
			}
			if i := strings.Index(p, "*"); i >= 0 && strings.Contains(p[i:], "/") {
				return nil, fmt.Errorf("area %q: path %q: a wildcard may only appear in the last path segment", a.Name, p)
			}
			if other, ok := pathArea[p]; ok {
				return nil, fmt.Errorf("area %q: path %q already appears in area %q", a.Name, p, other)
			}
			pathArea[p] = a.Name
		}
		byName[a.Name] = a
	}
	return byName, nil
}

// checkSortedUnique validates that a list of GitHub usernames is sorted
// case-insensitively and has no duplicates.
func checkSortedUnique(field string, logins []string) error {
	for i, l := range logins {
		if l == "" {
			return fmt.Errorf("%s: empty username", field)
		}
		if i > 0 && strings.ToLower(l) <= strings.ToLower(logins[i-1]) {
			return fmt.Errorf("%s must be sorted (case-insensitively) and unique: %q comes after %q", field, l, logins[i-1])
		}
	}
	return nil
}

// parseStatus splits a status into its kind ("ACTIVE", "HIATUS_SINCE" or
// "EMERITUS_SINCE") and date, and validates both.
func parseStatus(status string) (string, string, error) {
	kind, date, hasDate := strings.Cut(status, ":")
	if hasDate {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return "", "", fmt.Errorf("invalid date in status %q", status)
		}
	}
	switch {
	case kind == "ACTIVE" && !hasDate:
	case kind == "HIATUS_SINCE" && hasDate:
	case kind == "EMERITUS_SINCE" && hasDate:
	default:
		return "", "", fmt.Errorf("invalid status %q", status)
	}
	return kind, date, nil
}

// parseRoster parses and validates maintainers.yaml contents.
func parseRoster(yamlData []byte, areasByName map[string]area) (*roster, error) {
	dec := yaml.NewDecoder(bytes.NewReader(yamlData))
	dec.KnownFields(true)
	var r roster
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("cannot parse roster: %w", err)
	}
	seen := make(map[string]bool, len(r.Maintainers))
	seenIDs := make(map[uint64]bool, len(r.Maintainers))
	var prev maintainer
	for _, m := range r.Maintainers {
		if m.Name == "" || m.GitHub == "" || m.GitHubID == 0 || m.Affiliation == "" || m.Started == "" || m.Status == "" {
			return nil, fmt.Errorf("maintainer %+v: name, github, github_id, affiliation, started and status are required", m)
		}
		if _, err := time.Parse("2006-01-02", m.Started); err != nil {
			return nil, fmt.Errorf("maintainer %q: invalid started date %q", m.GitHub, m.Started)
		}
		for _, p := range m.PastAffiliations {
			if p.Affiliation == "" {
				return nil, fmt.Errorf("maintainer %q: past_affiliation entries require an affiliation", m.GitHub)
			}
			if _, err := time.Parse("2006-01-02", p.Until); err != nil {
				return nil, fmt.Errorf("maintainer %q: invalid until date %q in past_affiliation", m.GitHub, p.Until)
			}
		}
		if _, _, err := parseStatus(m.Status); err != nil {
			return nil, fmt.Errorf("maintainer %q: %w", m.GitHub, err)
		}
		if m.Started < prev.Started {
			return nil, fmt.Errorf("maintainers are not sorted by started date: %q (%s) comes after %q (%s)", m.Name, m.Started, prev.Name, prev.Started)
		}
		prev = m
		if seen[m.GitHub] {
			return nil, fmt.Errorf("duplicate maintainer %q", m.GitHub)
		}
		seen[m.GitHub] = true
		if seenIDs[m.GitHubID] {
			return nil, fmt.Errorf("maintainer %q: duplicate github_id %d", m.GitHub, m.GitHubID)
		}
		seenIDs[m.GitHubID] = true
	}
	// Every owner and approver must be a maintainer who still has merge
	// permissions. Moving someone to emeritus therefore requires removing
	// them from their areas in the same change.
	kindOf := make(map[string]string, len(r.Maintainers))
	for _, m := range r.Maintainers {
		k, _, _ := parseStatus(m.Status)
		kindOf[m.GitHub] = k
	}
	for _, name := range slices.Sorted(maps.Keys(areasByName)) {
		a := areasByName[name]
		for _, l := range a.Approvers {
			k, ok := kindOf[l]
			if !ok {
				return nil, fmt.Errorf("area %q: approver %q is not in maintainers.yaml", name, l)
			}
			if k == "EMERITUS_SINCE" {
				return nil, fmt.Errorf("area %q: approver %q is emeritus; emeritus maintainers may not own any path", name, l)
			}
		}
	}
	return &r, nil
}

// handles prefixes each GitHub username with "@".
func handles(logins []string) []string {
	out := make([]string, len(logins))
	for i, l := range logins {
		out[i] = "@" + l
	}
	return out
}

// generateCODEOWNERS renders `CODEOWNERS` contents. Only areas with
// enforced_review get a CODEOWNERS section; its reviewers are the area's
// approvers. Owners are rendered in MAINTAINERS.md.
func generateCODEOWNERS(areasByName map[string]area) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Generated from governance/maintainers.yaml and governance/areas.yaml by\n")
	b.WriteString("# //governance/tools/maintainers:maintainers_gen; do not edit directly.\n")
	for _, name := range slices.Sorted(maps.Keys(areasByName)) {
		a := areasByName[name]
		if !*a.EnforcedReview {
			continue
		}
		fmt.Fprintf(&b, "\n# Area: %s\n", name)
		for _, p := range slices.Sorted(slices.Values(a.Paths)) {
			// A directory gets a trailing slash so CODEOWNERS matches its
			// contents; a wildcard pattern is passed through as-is (CODEOWNERS
			// globs share the one-segment semantics of areas.yaml).
			if strings.Contains(p, "*") {
				fmt.Fprintf(&b, "%s %s\n", p, strings.Join(handles(a.Approvers), " "))
			} else {
				fmt.Fprintf(&b, "%s/ %s\n", p, strings.Join(handles(a.Approvers), " "))
			}
		}
	}
	return b.Bytes(), nil
}

// formatTable renders a markdown table with columns formatted.
func formatTable(headers []string, rows [][]string) string {
	numCols := len(headers)
	colWidths := make([]int, numCols)
	for i, h := range headers {
		colWidths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > colWidths[i] {
				colWidths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	// Header
	for i, h := range headers {
		if i > 0 {
			b.WriteString(" | ")
		}
		if i == numCols-1 {
			b.WriteString(h)
		} else {
			b.WriteString(h)
			b.WriteString(strings.Repeat(" ", colWidths[i]-len(h)))
		}
	}
	b.WriteString("\n")
	// Separator
	for i := 0; i < numCols; i++ {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(strings.Repeat("-", colWidths[i]))
	}
	b.WriteString("\n")
	// Rows
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString(" | ")
			}
			if i == numCols-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", colWidths[i]-len(cell)))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// generateMaintainersMD renders `MAINTAINERS.md` contents.
func generateMaintainersMD(r *roster, areasByName map[string]area) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<!-- Generated from governance/maintainers.yaml and governance/areas.yaml by\n")
	b.WriteString("     //governance/tools/maintainers:maintainers_gen; do not edit directly. -->\n\n")
	b.WriteString("The current maintainers of the gVisor project are listed below.\n\n")

	// Invert the areas so each maintainer's row can list where they approve
	// and which of those areas they own (marked with an asterisk).
	areasOf := make(map[string][]string)
	for _, name := range slices.Sorted(maps.Keys(areasByName)) {
		a := areasByName[name]
		for _, l := range a.Approvers {
			label := name
			if slices.Contains(a.Owners, l) {
				label += "*"
			}
			areasOf[l] = append(areasOf[l], label)
		}
	}

	var activeRows [][]string
	for _, m := range r.Maintainers {
		if kind, _, _ := parseStatus(m.Status); kind != "EMERITUS_SINCE" {
			activeRows = append(activeRows, []string{
				m.Name,
				fmt.Sprintf("[@%s](https://github.com/%s)", m.GitHub, m.GitHub),
				m.Affiliation,
				strings.Join(areasOf[m.GitHub], ", "),
			})
		}
	}
	b.WriteString(formatTable([]string{"Name", "GitHub ID", "Company/Organization", "Areas (* = owner)"}, activeRows))

	b.WriteString(`
## Specialization areas

Each area of the repository (see governance/areas.yaml for the paths) has
owners and approvers. **Owners** are accountable for the area: they curate who
may approve in it and write its standards. **Approvers** review and approve
changes in it; the reviewer assigner routes pull requests to them. Owners are
always approvers. An area marked *enforced* requires an approver's review
before merge (see CODEOWNERS): either because it is security- or
release-sensitive and its approver list is deliberately short, or because its
owners have confirmed the list and landed the area's standards.

`)

	var areaRows [][]string
	for _, name := range slices.Sorted(maps.Keys(areasByName)) {
		a := areasByName[name]
		enforced := ""
		if *a.EnforcedReview {
			enforced = "yes"
		}
		areaRows = append(areaRows, []string{
			name,
			strings.Join(handles(a.Owners), " "),
			strings.Join(handles(a.Approvers), " "),
			enforced,
		})
	}
	b.WriteString(formatTable([]string{"Area", "Owners", "Approvers", "Enforced"}, areaRows))

	b.WriteString(`
## Emeritus Maintainers

Maintainers who have stepped back from active participation are recognized here.
Emeritus maintainers do not have voting rights or merge access but are
recognized for their contributions and may be consulted on project matters.

A maintainer is moved to emeritus status after 12 months of inactivity, or by
request. An emeritus maintainer may return to active status through the normal
maintainer nomination process.

`)

	var emeritusRows [][]string
	for _, m := range r.Maintainers {
		if kind, date, _ := parseStatus(m.Status); kind == "EMERITUS_SINCE" {
			emeritusRows = append(emeritusRows, []string{
				m.Name,
				fmt.Sprintf("[@%s](https://github.com/%s)", m.GitHub, m.GitHub),
				date,
			})
		}
	}
	b.WriteString(formatTable([]string{"Name", "GitHub ID", "Date moved to emeritus"}, emeritusRows))

	return b.Bytes(), nil
}

func main() {
	flag.Parse()
	if *input == "" || *areasPath == "" {
		fmt.Fprintln(os.Stderr, "must specify -input and -areas")
		os.Exit(1)
	}
	areasData, err := os.ReadFile(*areasPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read areas: %v\n", err)
		os.Exit(1)
	}
	areasByName, err := parseAreas(areasData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot parse %s: %v\n", *areasPath, err)
		os.Exit(1)
	}
	yamlData, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read roster: %v\n", err)
		os.Exit(1)
	}
	r, err := parseRoster(yamlData, areasByName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot parse %s: %v\n", *input, err)
		os.Exit(1)
	}
	var outData []byte
	switch *format {
	case "MAINTAINERS.md":
		outData, err = generateMaintainersMD(r, areasByName)
	case "CODEOWNERS":
		outData, err = generateCODEOWNERS(areasByName)
	default:
		err = fmt.Errorf("invalid format %q", *format)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot generate output: %v\n", err)
		os.Exit(1)
	}
	if *output == "" {
		os.Stdout.Write(outData)
	} else if err := os.WriteFile(*output, outData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "cannot write %s: %v\n", *output, err)
		os.Exit(1)
	}
}
