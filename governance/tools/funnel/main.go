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

// Command funnel decides what happens to a pull request before a human is
// asked to look at it.
//
//	funnel floor   holds a pull request that lacks a description or an
//	               Assisted-by: line: label, one comment to the author,
//	               exit status 1. Clears the hold once they are present.
//
// It reads the pull request through the GitHub API and treats everything in
// it as data. Under GitHub Actions the repository and pull request come from
// the event; -repo and -pr override them, and -dry-run reports what would be
// done and changes nothing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v84/github"
)

var (
	repoFlag = flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "repository as owner/name (default $GITHUB_REPOSITORY)")
	prFlag   = flag.Int("pr", 0, "pull request number (default: the one in $GITHUB_EVENT_PATH)")
	dryRun   = flag.Bool("dry-run", false, "decide and report; change nothing on GitHub")
	enforce  = flag.Bool("enforce", os.Getenv("FLOOR_ENFORCE") == "true", "floor: hold pull requests that fail, instead of only reporting (default $FLOOR_ENFORCE == \"true\")")
)

// errHold is returned by the floor when it holds a pull request; main turns it
// into exit status 1 so the check shows red with the reason.
type errHold struct{ Floor }

func (e errHold) Error() string { return "held at the floor: " + strings.Join(e.Missing, "; ") }

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: funnel [flags] floor\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(0)

	owner, repo, ok := strings.Cut(*repoFlag, "/")
	if !ok {
		log.Fatalf("-repo must be owner/name, got %q", *repoFlag)
	}
	num := *prFlag
	if num == 0 {
		var err error
		if num, err = prFromEvent(os.Getenv("GITHUB_EVENT_PATH")); err != nil {
			log.Fatalf("no pull request: %v (pass -pr)", err)
		}
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	gh := github.NewClient(nil).WithAuthToken(token)
	ctx := context.Background()

	var err error
	switch cmd := flag.Arg(0); cmd {
	case "floor":
		err = runFloor(ctx, gh, owner, repo, num)
	default:
		log.Fatalf("unknown command %q", cmd)
	}
	var held errHold
	if errors.As(err, &held) {
		fmt.Printf("::error title=Held at the floor::%s\n", strings.Join(held.Missing, "; "))
		os.Exit(1)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// prFromEvent reads the pull request number from a GitHub Actions event file.
func prFromEvent(path string) (int, error) {
	if path == "" {
		return 0, errors.New("$GITHUB_EVENT_PATH is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var event struct {
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if event.PullRequest.Number == 0 {
		return 0, fmt.Errorf("%s has no pull_request.number", path)
	}
	return event.PullRequest.Number, nil
}

// runFloor checks the floor and, when enforcing, holds or releases the pull
// request accordingly.
func runFloor(ctx context.Context, gh *github.Client, owner, repo string, num int) error {
	pr, _, err := gh.PullRequests.Get(ctx, owner, repo, num)
	if err != nil {
		return err
	}
	if pr.GetUser().GetType() == "Bot" {
		setOutput("verdict", "skip")
		summary("Bot author; nothing to do.\n")
		return nil
	}

	var commits []string
	for page := 1; page != 0; {
		cs, resp, err := gh.PullRequests.ListCommits(ctx, owner, repo, num, &github.ListOptions{PerPage: 100, Page: page})
		if err != nil {
			return fmt.Errorf("listing commits: %w", err)
		}
		for _, c := range cs {
			commits = append(commits, c.GetCommit().GetMessage())
		}
		page = resp.NextPage
	}

	f := CheckFloor(pr.GetBody(), commits)
	verdict := "assign"
	if !f.Clear() {
		verdict = "hold"
	}
	summary("## Floor: %s\n\n", verdict)
	if f.Clear() {
		summary("Description and `Assisted-by:` present.\n")
	} else {
		summary("Missing:\n")
		for _, m := range f.Missing {
			summary("- %s\n", m)
		}
	}
	summary("\nSize: +%d −%d across %d files.\n", pr.GetAdditions(), pr.GetDeletions(), pr.GetChangedFiles())
	switch {
	case *dryRun:
		summary("\n_Dry run: nothing changed._\n")
	case !*enforce:
		summary("\n_Not enforcing (FLOOR_ENFORCE is not \"true\"): no label or comment; reviewers are assigned regardless._\n")
	}

	if !f.Clear() && *enforce {
		if !*dryRun {
			if err := hold(ctx, gh, owner, repo, num, f); err != nil {
				return err
			}
		}
		setOutput("verdict", "hold")
		return errHold{f}
	}
	if !*dryRun {
		if err := release(ctx, gh, owner, repo, num); err != nil {
			return err
		}
	}
	setOutput("verdict", "assign")
	return nil
}

// hold labels the pull request and leaves, or refreshes, the floor's comment.
func hold(ctx context.Context, gh *github.Client, owner, repo string, num int, f Floor) error {
	if _, _, err := gh.Issues.GetLabel(ctx, owner, repo, HoldLabel); isNotFound(err) {
		if _, _, err := gh.Issues.CreateLabel(ctx, owner, repo, &github.Label{
			Name:        github.Ptr(HoldLabel),
			Color:       github.Ptr("d93f0b"),
			Description: github.Ptr("Held at the floor; no reviewer is asked until the author addresses the comment."),
		}); err != nil {
			return fmt.Errorf("creating label: %w", err)
		}
	}
	if _, _, err := gh.Issues.AddLabelsToIssue(ctx, owner, repo, num, []string{HoldLabel}); err != nil {
		return fmt.Errorf("adding label: %w", err)
	}
	body := github.Ptr(holdComment(f))
	id, err := findHoldComment(ctx, gh, owner, repo, num)
	if err != nil {
		return err
	}
	if id != 0 {
		_, _, err = gh.Issues.EditComment(ctx, owner, repo, id, &github.IssueComment{Body: body})
	} else {
		_, _, err = gh.Issues.CreateComment(ctx, owner, repo, num, &github.IssueComment{Body: body})
	}
	if err != nil {
		return fmt.Errorf("commenting: %w", err)
	}
	return nil
}

// release removes the floor's label and comment, if present.
func release(ctx context.Context, gh *github.Client, owner, repo string, num int) error {
	if _, err := gh.Issues.RemoveLabelForIssue(ctx, owner, repo, num, HoldLabel); err != nil && !isNotFound(err) {
		return fmt.Errorf("removing label: %w", err)
	}
	id, err := findHoldComment(ctx, gh, owner, repo, num)
	if err != nil {
		return err
	}
	if id != 0 {
		if _, err := gh.Issues.DeleteComment(ctx, owner, repo, id); err != nil {
			return fmt.Errorf("deleting comment: %w", err)
		}
	}
	return nil
}

// findHoldComment returns the id of the floor's comment, or 0.
func findHoldComment(ctx context.Context, gh *github.Client, owner, repo string, num int) (int64, error) {
	for page := 1; page != 0; {
		cs, resp, err := gh.Issues.ListComments(ctx, owner, repo, num, &github.IssueListCommentsOptions{
			ListOptions: github.ListOptions{PerPage: 100, Page: page},
		})
		if err != nil {
			return 0, fmt.Errorf("listing comments: %w", err)
		}
		for _, c := range cs {
			if c.GetUser().GetLogin() == "github-actions[bot]" && isHoldComment(c.GetBody()) {
				return c.GetID(), nil
			}
		}
		page = resp.NextPage
	}
	return 0, nil
}

// isNotFound reports whether err is a GitHub 404.
func isNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

// setOutput sets a GitHub Actions step output, when running under Actions.
func setOutput(key, value string) {
	appendFile(os.Getenv("GITHUB_OUTPUT"), key+"="+value+"\n")
}

// summary writes to the GitHub Actions job summary, and to stdout.
func summary(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	fmt.Print(s)
	appendFile(os.Getenv("GITHUB_STEP_SUMMARY"), s)
}

func appendFile(path, s string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		log.Printf("Notice: %v", err)
		return
	}
	defer f.Close()
	f.WriteString(s)
}
