/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"flag"
	"fmt"
	"net/http"
	"time"

	"k8s.io/kubernetes/pkg/util"

	"github.com/golang/glog"
	"github.com/google/go-github/github"
	"github.com/gregjones/httpcache"
	"golang.org/x/oauth2"
)

var (
	useMemoryCache = flag.Bool("use-http-cache", false, "If true, use a client side HTTP cache for API requests.")
)

const (
	NeedsOKToMergeLabel = "needs-ok-to-merge"
)

type RateLimitRoundTripper struct {
	delegate http.RoundTripper
	throttle util.RateLimiter
}

func (r *RateLimitRoundTripper) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	r.throttle.Accept()
	return r.delegate.RoundTrip(req)
}

func MakeClient(token string) *github.Client {
	var client *http.Client
	var transport http.RoundTripper
	if *useMemoryCache {
		transport = httpcache.NewMemoryCacheTransport()
	} else {
		transport = http.DefaultTransport
	}
	if len(token) > 0 {
		rateLimitTransport := &RateLimitRoundTripper{
			delegate: transport,
			// Global limit is 5000 Q/Hour, try to only use 1800 to make room for other apps
			throttle: util.NewTokenBucketRateLimiter(0.5, 10),
		}
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		client = &http.Client{
			Transport: &oauth2.Transport{
				Base:   rateLimitTransport,
				Source: oauth2.ReuseTokenSource(nil, ts),
			},
		}
	} else {
		rateLimitTransport := &RateLimitRoundTripper{
			delegate: transport,
			throttle: util.NewTokenBucketRateLimiter(0.01, 10),
		}
		client = &http.Client{
			Transport: rateLimitTransport,
		}
	}
	return github.NewClient(client)
}

func HasLabel(labels []github.Label, name string) bool {
	for i := range labels {
		label := &labels[i]
		if label.Name != nil && *label.Name == name {
			return true
		}
	}
	return false
}

func HasLabels(labels []github.Label, names []string) bool {
	for i := range names {
		if !HasLabel(labels, names[i]) {
			return false
		}
	}
	return true
}

func fetchAllUsers(client *github.Client, team int) ([]github.User, error) {
	page := 1
	var result []github.User
	for {
		glog.V(4).Infof("Fetching page %d of all users", page)
		listOpts := &github.OrganizationListTeamMembersOptions{
			ListOptions: github.ListOptions{PerPage: 100, Page: page},
		}
		users, response, err := client.Organizations.ListTeamMembers(team, listOpts)
		if err != nil {
			return nil, err
		}
		result = append(result, users...)
		if response.LastPage == 0 || response.LastPage == page {
			break
		}
		page++
	}
	return result, nil
}

func fetchAllTeams(client *github.Client, org string) ([]github.Team, error) {
	page := 1
	var result []github.Team
	for {
		glog.V(4).Infof("Fetching page %d of all teams", page)
		listOpts := &github.ListOptions{PerPage: 100, Page: page}
		teams, response, err := client.Organizations.ListTeams(org, listOpts)
		if err != nil {
			return nil, err
		}
		result = append(result, teams...)
		if response.LastPage == 0 || response.LastPage == page {
			break
		}
		page++
	}
	return result, nil
}

// Get PRs (which are issues) via the issues api, so that we can filter by labels, greatly speeding up the queue.
// Non-PR issues will be filtered out.
func fetchAllPRsWithLabels(client *github.Client, user, project string, labels []string) ([]github.Issue, error) {
	page := 1
	var result []github.Issue
	for {
		glog.V(4).Infof("Fetching page %d", page)
		listOpts := &github.IssueListByRepoOptions{
			Sort:        "created",
			Labels:      labels,
			State:       "open",
			ListOptions: github.ListOptions{PerPage: 100, Page: page},
		}
		issues, response, err := client.Issues.ListByRepo(user, project, listOpts)
		if err != nil {
			return nil, err
		}
		for i := range issues {
			issue := &issues[i]
			if issue.PullRequestLinks != nil {
				result = append(result, *issue)
			}
		}
		if response.LastPage == 0 || response.LastPage == page {
			break
		}
		page++
	}
	return result, nil
}

func fetchAllPRs(client *github.Client, user, project string) ([]github.PullRequest, error) {
	page := 1
	var result []github.PullRequest
	for {
		glog.V(4).Infof("Fetching page %d", page)
		listOpts := &github.PullRequestListOptions{
			Sort:        "desc",
			ListOptions: github.ListOptions{PerPage: 100, Page: page},
		}
		prs, response, err := client.PullRequests.List(user, project, listOpts)
		if err != nil {
			return nil, err
		}
		result = append(result, prs...)
		if response.LastPage == 0 || response.LastPage == page {
			break
		}
		page++
	}
	return result, nil
}

type PRFunction func(*github.Client, *github.PullRequest, *github.Issue) error

type FilterConfig struct {
	MinPRNumber int

	// non-committer users believed safe
	AdditionalUserWhitelist []string

	// Committers are static here in case they can't be gotten dynamically;
	// they do not need to be whitelisted.
	Committers []string

	// The label needed to override absence from whitelist.
	WhitelistOverride string

	// If true, don't make any mutating API calls
	DryRun                 bool
	DontRequireE2ELabel    string
	E2EStatusContext       string
	RequiredStatusContexts []string

	// Private, cached
	userWhitelist util.StringSet
}

func lastModifiedTime(client *github.Client, user, project string, pr *github.PullRequest) (*time.Time, error) {
	list, _, err := client.PullRequests.ListCommits(user, project, *pr.Number, &github.ListOptions{})
	if err != nil {
		return nil, err
	}
	var lastModified *time.Time
	for ix := range list {
		item := list[ix]
		if lastModified == nil || item.Commit.Committer.Date.After(*lastModified) {
			lastModified = item.Commit.Committer.Date
		}
	}
	return lastModified, nil
}

func GetAllEventsForPR(client *github.Client, user, project string, prNumber int) ([]github.IssueEvent, error) {
	events := []github.IssueEvent{}
	page := 1
	for {
		eventPage, response, err := client.Issues.ListIssueEvents(user, project, prNumber, &github.ListOptions{Page: page})
		if err != nil {
			glog.Errorf("Error getting events for issue: %v", err)
			return nil, err
		}
		events = append(events, eventPage...)
		if response.LastPage == 0 || response.LastPage == page {
			break
		}
		page++
	}
	return events, nil
}

func validateLGTMAfterPush(client *github.Client, user, project string, pr *github.PullRequest, lastModifiedTime *time.Time) (bool, error) {
	var lgtmTime *time.Time
	events, err := GetAllEventsForPR(client, user, project, *pr.Number)
	if err != nil {
		return false, err
	}
	for ix := range events {
		event := &events[ix]
		if *event.Event == "labeled" && *event.Label.Name == "lgtm" {
			if lgtmTime == nil || event.CreatedAt.After(*lgtmTime) {
				lgtmTime = event.CreatedAt
			}
		}
	}
	if lgtmTime == nil {
		return false, fmt.Errorf("Couldn't find time for LGTM label, this shouldn't happen, skipping PR: %d", *pr.Number)
	}
	return lastModifiedTime.Before(*lgtmTime), nil
}

func UsersWithCommit(client *github.Client, org, project string) ([]string, error) {
	userSet := util.StringSet{}

	teams, err := fetchAllTeams(client, org)
	if err != nil {
		glog.Errorf("%v", err)
		return nil, err
	}

	teamIDs := []int{}
	for _, team := range teams {
		repo, _, err := client.Organizations.IsTeamRepo(*team.ID, org, project)
		if repo == nil || err != nil {
			continue
		}
		perms := *repo.Permissions
		if perms["push"] {
			teamIDs = append(teamIDs, *team.ID)
		}
	}

	for _, team := range teamIDs {
		users, err := fetchAllUsers(client, team)
		if err != nil {
			glog.Errorf("%v", err)
			continue
		}
		for _, user := range users {
			userSet.Insert(*user.Login)
		}
	}

	return userSet.List(), nil
}

// RefreshWhitelist updates the whitelist, re-getting the list of committers.
func (config *FilterConfig) RefreshWhitelist(client *github.Client, user, project string) util.StringSet {
	userSet := util.StringSet{}
	userSet.Insert(config.AdditionalUserWhitelist...)
	if usersWithCommit, err := UsersWithCommit(client, user, project); err != nil {
		glog.Info("Falling back to static committers list.")
		// Use the static list if there was an error getting the list dynamically
		userSet.Insert(config.Committers...)
	} else {
		userSet.Insert(usersWithCommit...)
	}
	config.userWhitelist = userSet
	return userSet
}

// For each PR in the project that matches:
//   * pr.Number > minPRNumber
//   * is mergeable
//   * has labels "cla: yes", "lgtm"
//   * combinedStatus = 'success' (e.g. all hooks have finished success in github)
// Run the specified function
func ForEachCandidatePRDo(client *github.Client, user, project string, fn PRFunction, once bool, config *FilterConfig) error {
	// Get all PRs that have lgtm and cla: yes labels
	issues, err := fetchAllPRsWithLabels(client, user, project, []string{"lgtm", "cla: yes"})
	if err != nil {
		return err
	}

	if config.userWhitelist == nil {
		config.RefreshWhitelist(client, user, project)
	}

	userSet := config.userWhitelist

	for ix := range issues {
		issue := &issues[ix]
		if issue.User == nil || issue.User.Login == nil {
			glog.V(2).Infof("Skipping PR %d with no user info %#v.", *issue.Number, issue.User)
			continue
		}
		if *issue.Number < config.MinPRNumber {
			glog.V(6).Infof("Dropping %d < %d", *issue.Number, config.MinPRNumber)
			continue
		}
		glog.V(2).Infof("----==== %d ====----", *issue.Number)

		glog.V(8).Infof("%v", issue.Labels)
		if !HasLabels(issue.Labels, []string{"lgtm", "cla: yes"}) {
			glog.V(2).Infof("Skipping %d - doesn't have requisite labels", *issue.Number)
			continue
		}

		pr, _, err := client.PullRequests.Get(user, project, *issue.Number)
		if err != nil {
			glog.Errorf("Error getting pull request: %v", err)
			continue
		}

		if !HasLabel(issue.Labels, config.WhitelistOverride) && !userSet.Has(*pr.User.Login) {
			glog.V(4).Infof("Dropping %d since %s isn't in whitelist and %s isn't present", *pr.Number, *pr.User.Login, config.WhitelistOverride)
			if config.DryRun {
				glog.Infof("PR %d: would have asked for ok-to-merge but DryRun is true", *pr.Number)
				continue
			}
			if !HasLabel(issue.Labels, NeedsOKToMergeLabel) {
				if _, _, err := client.Issues.AddLabelsToIssue(user, project, *pr.Number, []string{NeedsOKToMergeLabel}); err != nil {
					glog.Errorf("Failed to set 'needs-ok-to-merge' for %d", *pr.Number)
				}
				body := "The author of this PR is not in the whitelist for merge, can one of the admins add the 'ok-to-merge' label?"
				if _, _, err := client.Issues.CreateComment(user, project, *pr.Number, &github.IssueComment{Body: &body}); err != nil {
					glog.Errorf("Failed to add a comment for %d", *pr.Number)
				}
			}
			continue
		}

		// Tidy up the issue list.
		if HasLabel(issue.Labels, NeedsOKToMergeLabel) && !config.DryRun {
			if _, err := client.Issues.RemoveLabelForIssue(user, project, *pr.Number, NeedsOKToMergeLabel); err != nil {
				glog.Warningf("Failed to remove 'needs-ok-to-merge' from issue %d, which doesn't need it", *pr.Number)
			}
		}

		lastModifiedTime, err := lastModifiedTime(client, user, project, pr)
		if err != nil {
			glog.Errorf("Failed to get last modified time, skipping PR: %d", *pr.Number)
			continue
		}
		if ok, err := validateLGTMAfterPush(client, user, project, pr, lastModifiedTime); err != nil {
			glog.Errorf("Error validating LGTM: %v, Skipping: %d", err, *pr.Number)
			continue
		} else if !ok {
			if config.DryRun {
				glog.Info("PR was pushed after LGTM, would have removed LGTM, but DryRun is true")
				continue
			}
			glog.Errorf("PR pushed after LGTM, attempting to remove LGTM and skipping")
			staleLGTMBody := "LGTM was before last commit, removing LGTM"
			if _, _, err := client.Issues.CreateComment(user, project, *pr.Number, &github.IssueComment{Body: &staleLGTMBody}); err != nil {
				glog.Warningf("Failed to create remove label comment: %v", err)
			}
			if _, err := client.Issues.RemoveLabelForIssue(user, project, *pr.Number, "lgtm"); err != nil {
				glog.Warningf("Failed to remove 'lgtm' label for stale lgtm on %d", *pr.Number)
			}
			continue
		}

		// This is annoying, github appears to only temporarily cache mergeability, if it is nil, wait
		// for an async refresh and retry.
		if pr.Mergeable == nil {
			glog.Infof("Waiting for mergeability on %s %d", *pr.Title, *pr.Number)
			// TODO: determine what a good empirical setting for this is.
			time.Sleep(10 * time.Second)
			pr, _, err = client.PullRequests.Get(user, project, *pr.Number)
		}
		if pr.Mergeable == nil {
			glog.Errorf("No mergeability information for %s %d, Skipping.", *pr.Title, *pr.Number)
			continue
		}
		if !*pr.Mergeable {
			glog.V(2).Infof("Skipping %d - not mergable", *pr.Number)
			continue
		}

		// Validate the status information for this PR
		contexts := config.RequiredStatusContexts
		if len(config.DontRequireE2ELabel) == 0 || !HasLabel(issue.Labels, config.DontRequireE2ELabel) {
			contexts = append(contexts, config.E2EStatusContext)
		}
		ok, err := ValidateStatus(client, user, project, *pr.Number, contexts, false)
		if err != nil {
			glog.Errorf("Error validating PR status: %v", err)
			continue
		}
		if !ok {
			continue
		}
		if err := fn(client, pr, issue); err != nil {
			glog.Errorf("Failed to run user function: %v", err)
			break
		}
		if once {
			break
		}
	}
	return nil
}

func getCommitStatus(client *github.Client, user, project string, prNumber int) ([]*github.CombinedStatus, error) {
	commits, _, err := client.PullRequests.ListCommits(user, project, prNumber, &github.ListOptions{})
	if err != nil {
		return nil, err
	}
	commitStatus := make([]*github.CombinedStatus, len(commits))
	for ix := range commits {
		commit := &commits[ix]
		statusList, _, err := client.Repositories.GetCombinedStatus(user, project, *commit.SHA, &github.ListOptions{})
		if err != nil {
			return nil, err
		}
		commitStatus[ix] = statusList
	}
	return commitStatus, nil
}

// Gets the current status of a PR by introspecting the status of the commits in the PR.
// The rules are:
//    * If any member of the 'requiredContexts' list is missing, it is 'incomplete'
//    * If any commit is 'pending', the PR is 'pending'
//    * If any commit is 'error', the PR is in 'error'
//    * If any commit is 'failure', the PR is 'failure'
//    * Otherwise the PR is 'success'
func GetStatus(client *github.Client, user, project string, prNumber int, requiredContexts []string) (string, error) {
	statusList, err := getCommitStatus(client, user, project, prNumber)
	if err != nil {
		return "", err
	}
	return computeStatus(statusList, requiredContexts), nil
}

func computeStatus(statusList []*github.CombinedStatus, requiredContexts []string) string {
	states := util.StringSet{}
	providers := util.StringSet{}
	for ix := range statusList {
		status := statusList[ix]
		glog.V(8).Infof("Checking commit: %s", *status.SHA)
		glog.V(8).Infof("Checking commit: %v", status)
		states.Insert(*status.State)

		for _, subStatus := range status.Statuses {
			glog.V(8).Infof("Found status from: %v", subStatus)
			providers.Insert(*subStatus.Context)
		}
	}
	for _, provider := range requiredContexts {
		if !providers.Has(provider) {
			glog.V(8).Infof("Failed to find %s in %v", provider, providers)
			return "incomplete"
		}
	}

	switch {
	case states.Has("pending"):
		return "pending"
	case states.Has("error"):
		return "error"
	case states.Has("failure"):
		return "failure"
	default:
		return "success"
	}
}

// Make sure that the combined status for all commits in a PR is 'success'
// if 'waitForPending' is true, this function will wait until the PR is no longer pending (all checks have run)
func ValidateStatus(client *github.Client, user, project string, prNumber int, requiredContexts []string, waitOnPending bool) (bool, error) {
	pending := true
	for pending {
		status, err := GetStatus(client, user, project, prNumber, requiredContexts)
		if err != nil {
			return false, err
		}
		switch status {
		case "error", "failure":
			return false, nil
		case "pending":
			if !waitOnPending {
				return false, nil
			}
			pending = true
			glog.V(4).Info("PR is pending, waiting for 30 seconds")
			time.Sleep(30 * time.Second)
		case "success":
			return true, nil
		case "incomplete":
			return false, nil
		default:
			return false, fmt.Errorf("unknown status: %s", status)
		}
	}
	return true, nil
}

// Wait for a PR to move into Pending.  This is useful because the request to test a PR again
// is asynchronous with the PR actually moving into a pending state
// TODO: add a timeout
func WaitForPending(client *github.Client, user, project string, prNumber int) error {
	for {
		status, err := GetStatus(client, user, project, prNumber, []string{})
		if err != nil {
			return err
		}
		if status == "pending" {
			return nil
		}
		glog.V(4).Info("PR is not pending, waiting for 30 seconds")
		time.Sleep(30 * time.Second)
	}
}
