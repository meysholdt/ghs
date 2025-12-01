package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

// getTokenFromGitCredential retrieves a GitHub token from the git credential helper
func getTokenFromGitCredential() (string, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git credential fill failed: %w: %s", err, stderr.String())
	}

	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "password=") {
			return strings.TrimPrefix(line, "password="), nil
		}
	}

	return "", fmt.Errorf("no password found in git credential output")
}

func main() {
	org := flag.String("org", "", "GitHub organization name (required)")
	token := flag.String("token", "", "GitHub personal access token (falls back to GITHUB_TOKEN env var, then git credential helper)")
	output := flag.String("output", "output.md", "Output markdown file path")
	flag.Parse()

	// Token resolution order: flag > env var > git credential helper
	tokenValue := *token
	if tokenValue == "" {
		tokenValue = os.Getenv("GITHUB_TOKEN")
	}
	if tokenValue == "" {
		var err error
		tokenValue, err = getTokenFromGitCredential()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not get token from git credential helper: %v\n", err)
		}
	}

	if *org == "" {
		fmt.Fprintln(os.Stderr, "Error: -org is required")
		flag.Usage()
		os.Exit(1)
	}
	if tokenValue == "" {
		fmt.Fprintln(os.Stderr, "Error: no token provided. Use -token flag, GITHUB_TOKEN env var, or configure git credential helper")
		flag.Usage()
		os.Exit(1)
	}

	ctx := context.Background()
	client := newGitHubClient(ctx, tokenValue)

	fmt.Println("Fetching organization members...")
	orgMembers, err := fetchOrgMembers(ctx, client, *org)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching org members: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching user emails...")
	userEmails, emailsAvailable := fetchUserEmails(ctx, client, orgMembers)

	fmt.Println("Fetching teams...")
	teams, err := fetchAllTeams(ctx, client, *org)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching teams: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching team members...")
	teamMembers, err := fetchTeamMembers(ctx, client, *org, teams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching team members: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching team hierarchy...")
	teamChildren := buildTeamHierarchy(teams)

	fmt.Println("Fetching repositories...")
	repos, err := fetchAllRepos(ctx, client, *org)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching repositories: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching repository access...")
	repoAccess, err := fetchRepoAccess(ctx, client, *org, repos)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching repository access: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Generating markdown...")
	markdown := generateMarkdown(*org, teams, teamMembers, teamChildren, repos, repoAccess, orgMembers, userEmails, emailsAvailable)

	if err := os.WriteFile(*output, []byte(markdown), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Output written to %s\n", *output)
}

func newGitHubClient(ctx context.Context, token string) *github.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	return github.NewClient(tc)
}

func handleRateLimit(resp *github.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.Rate.Remaining == 0 {
		sleepDuration := time.Until(resp.Rate.Reset.Time) + time.Second
		fmt.Printf("Rate limit exceeded. Sleeping for %v...\n", sleepDuration)
		time.Sleep(sleepDuration)
		return nil // Signal to retry
	}
	if rateLimitErr, ok := err.(*github.RateLimitError); ok {
		sleepDuration := time.Until(rateLimitErr.Rate.Reset.Time) + time.Second
		fmt.Printf("Rate limit exceeded. Sleeping for %v...\n", sleepDuration)
		time.Sleep(sleepDuration)
		return nil // Signal to retry
	}
	if abuseErr, ok := err.(*github.AbuseRateLimitError); ok {
		sleepDuration := abuseErr.GetRetryAfter()
		if sleepDuration == 0 {
			sleepDuration = time.Minute
		}
		fmt.Printf("Abuse rate limit. Sleeping for %v...\n", sleepDuration)
		time.Sleep(sleepDuration)
		return nil // Signal to retry
	}
	return err
}

func fetchOrgMembers(ctx context.Context, client *github.Client, org string) ([]*github.User, error) {
	var allMembers []*github.User
	opts := &github.ListMembersOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		members, resp, err := client.Organizations.ListMembers(ctx, org, opts)
		if retryErr := handleRateLimit(resp, err); retryErr != nil {
			return nil, retryErr
		} else if err != nil {
			continue // Retry after rate limit sleep
		}

		allMembers = append(allMembers, members...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allMembers, nil
}

// fetchUserEmails fetches email addresses for users. Returns a map of login->email and whether all emails were available.
func fetchUserEmails(ctx context.Context, client *github.Client, users []*github.User) (map[string]string, bool) {
	emails := make(map[string]string)
	allAvailable := true

	for _, user := range users {
		// Fetch full user details to get email
		fullUser, resp, err := client.Users.Get(ctx, user.GetLogin())
		if err != nil {
			handleRateLimit(resp, err)
			// If we can't get email for any user, mark as not all available
			allAvailable = false
			continue
		}

		email := fullUser.GetEmail()
		if email != "" {
			emails[user.GetLogin()] = email
		} else {
			allAvailable = false
		}
	}

	return emails, allAvailable
}

func fetchAllTeams(ctx context.Context, client *github.Client, org string) ([]*github.Team, error) {
	var allTeams []*github.Team
	opts := &github.ListOptions{PerPage: 100}

	for {
		teams, resp, err := client.Teams.ListTeams(ctx, org, opts)
		if retryErr := handleRateLimit(resp, err); retryErr != nil {
			return nil, retryErr
		} else if err != nil {
			continue // Retry after rate limit sleep
		}

		allTeams = append(allTeams, teams...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allTeams, nil
}

func fetchTeamMembers(ctx context.Context, client *github.Client, org string, teams []*github.Team) (map[int64][]*github.User, error) {
	members := make(map[int64][]*github.User)

	for _, team := range teams {
		opts := &github.TeamListTeamMembersOptions{ListOptions: github.ListOptions{PerPage: 100}}
		var teamMembers []*github.User

		for {
			users, resp, err := client.Teams.ListTeamMembersBySlug(ctx, org, team.GetSlug(), opts)
			if retryErr := handleRateLimit(resp, err); retryErr != nil {
				return nil, retryErr
			} else if err != nil {
				continue // Retry after rate limit sleep
			}

			teamMembers = append(teamMembers, users...)
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}

		members[team.GetID()] = teamMembers
	}

	return members, nil
}

func buildTeamHierarchy(teams []*github.Team) map[int64][]int64 {
	children := make(map[int64][]int64)
	for _, team := range teams {
		if team.Parent != nil {
			parentID := team.Parent.GetID()
			children[parentID] = append(children[parentID], team.GetID())
		}
	}
	return children
}

// getAllMembers returns all members of a team including nested team members
func getAllMembers(teamID int64, teamMembers map[int64][]*github.User, teamChildren map[int64][]int64, visited map[int64]bool) []*github.User {
	if visited[teamID] {
		return nil
	}
	visited[teamID] = true

	memberSet := make(map[string]*github.User)

	// Add direct members
	for _, member := range teamMembers[teamID] {
		memberSet[member.GetLogin()] = member
	}

	// Add members from child teams recursively
	for _, childID := range teamChildren[teamID] {
		childMembers := getAllMembers(childID, teamMembers, teamChildren, visited)
		for _, member := range childMembers {
			memberSet[member.GetLogin()] = member
		}
	}

	result := make([]*github.User, 0, len(memberSet))
	for _, member := range memberSet {
		result = append(result, member)
	}

	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].GetLogin()) < strings.ToLower(result[j].GetLogin())
	})

	return result
}

func fetchAllRepos(ctx context.Context, client *github.Client, org string) ([]*github.Repository, error) {
	var allRepos []*github.Repository
	opts := &github.RepositoryListByOrgOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		repos, resp, err := client.Repositories.ListByOrg(ctx, org, opts)
		if retryErr := handleRateLimit(resp, err); retryErr != nil {
			return nil, retryErr
		} else if err != nil {
			continue // Retry after rate limit sleep
		}

		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allRepos, nil
}

type RepoAccess struct {
	Teams         []*github.Team
	Collaborators []*github.User
}

// isNotFoundError checks if the error is a 404 Not Found
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errResp, ok := err.(*github.ErrorResponse); ok {
		return errResp.Response.StatusCode == 404
	}
	return false
}

func fetchRepoAccess(ctx context.Context, client *github.Client, org string, repos []*github.Repository) (map[string]*RepoAccess, error) {
	access := make(map[string]*RepoAccess)

	for _, repo := range repos {
		repoName := repo.GetName()
		access[repoName] = &RepoAccess{}

		// Fetch teams with access
		teamOpts := &github.ListOptions{PerPage: 100}
	teamLoop:
		for {
			teams, resp, err := client.Repositories.ListTeams(ctx, org, repoName, teamOpts)
			if isNotFoundError(err) {
				break teamLoop // Skip this repo's teams
			}
			if retryErr := handleRateLimit(resp, err); retryErr != nil {
				return nil, retryErr
			} else if err != nil {
				continue // Retry after rate limit sleep
			}

			access[repoName].Teams = append(access[repoName].Teams, teams...)
			if resp.NextPage == 0 {
				break
			}
			teamOpts.Page = resp.NextPage
		}

		// Fetch collaborators
		collabOpts := &github.ListCollaboratorsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	collabLoop:
		for {
			collaborators, resp, err := client.Repositories.ListCollaborators(ctx, org, repoName, collabOpts)
			if isNotFoundError(err) {
				break collabLoop // Skip this repo's collaborators
			}
			if retryErr := handleRateLimit(resp, err); retryErr != nil {
				return nil, retryErr
			} else if err != nil {
				continue // Retry after rate limit sleep
			}

			access[repoName].Collaborators = append(access[repoName].Collaborators, collaborators...)
			if resp.NextPage == 0 {
				break
			}
			collabOpts.Page = resp.NextPage
		}
	}

	return access, nil
}

// writeMembersTable writes a markdown table of members with username and email columns
func writeMembersTable(sb *strings.Builder, members []*github.User, userEmails map[string]string) {
	sb.WriteString("| Username | Email |\n")
	sb.WriteString("|----------|-------|\n")
	for _, member := range members {
		email := userEmails[member.GetLogin()]
		if email == "" {
			email = "-"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s |\n", member.GetLogin(), email))
	}
	sb.WriteString("\n")
}

func generateMarkdown(org string, teams []*github.Team, teamMembers map[int64][]*github.User, teamChildren map[int64][]int64, repos []*github.Repository, repoAccess map[string]*RepoAccess, orgMembers []*github.User, userEmails map[string]string, emailsAvailable bool) string {
	var sb strings.Builder

	// Build org members set for quick lookup
	orgMemberSet := make(map[string]bool)
	for _, member := range orgMembers {
		orgMemberSet[member.GetLogin()] = true
	}

	// Build team lookup by ID
	teamByID := make(map[int64]*github.Team)
	for _, team := range teams {
		teamByID[team.GetID()] = team
	}

	everybodyGroupName := fmt.Sprintf("everybody in %s", org)

	// Section 1: Groups
	sb.WriteString("# Groups\n\n")

	// First, list the implicit "everybody" group
	sb.WriteString(fmt.Sprintf("## %s\n\n", everybodyGroupName))
	sortedOrgMembers := make([]*github.User, len(orgMembers))
	copy(sortedOrgMembers, orgMembers)
	sort.Slice(sortedOrgMembers, func(i, j int) bool {
		return strings.ToLower(sortedOrgMembers[i].GetLogin()) < strings.ToLower(sortedOrgMembers[j].GetLogin())
	})
	writeMembersTable(&sb, sortedOrgMembers, userEmails)

	// Sort teams by name
	sortedTeams := make([]*github.Team, len(teams))
	copy(sortedTeams, teams)
	sort.Slice(sortedTeams, func(i, j int) bool {
		return strings.ToLower(sortedTeams[i].GetName()) < strings.ToLower(sortedTeams[j].GetName())
	})

	for _, team := range sortedTeams {
		sb.WriteString(fmt.Sprintf("## %s\n\n", team.GetName()))

		// Get all members including nested
		visited := make(map[int64]bool)
		allMembers := getAllMembers(team.GetID(), teamMembers, teamChildren, visited)

		if len(allMembers) == 0 {
			sb.WriteString("*No members*\n\n")
		} else {
			writeMembersTable(&sb, allMembers, userEmails)
		}
	}

	// Section 2: Projects
	sb.WriteString("# Projects\n\n")

	// Sort repos by name
	sortedRepos := make([]*github.Repository, len(repos))
	copy(sortedRepos, repos)
	sort.Slice(sortedRepos, func(i, j int) bool {
		return strings.ToLower(sortedRepos[i].GetName()) < strings.ToLower(sortedRepos[j].GetName())
	})

	// Write projects table
	sb.WriteString("| Name | Shared With |\n")
	sb.WriteString("|------|-------------|\n")

	for _, repo := range sortedRepos {
		repoName := repo.GetName()
		access := repoAccess[repoName]

		// Check if all org members have access (everybody group)
		everybodyHasAccess := true
		collaboratorSet := make(map[string]bool)
		for _, collab := range access.Collaborators {
			collaboratorSet[collab.GetLogin()] = true
		}
		for _, member := range orgMembers {
			if !collaboratorSet[member.GetLogin()] {
				everybodyHasAccess = false
				break
			}
		}

		// Collect all users covered by listed groups
		coveredUsers := make(map[string]bool)
		var sharedWith []string

		if everybodyHasAccess {
			sharedWith = append(sharedWith, everybodyGroupName)
			// All org members are covered
			for _, member := range orgMembers {
				coveredUsers[member.GetLogin()] = true
			}
		}

		// Add teams with access
		sortedAccessTeams := make([]*github.Team, len(access.Teams))
		copy(sortedAccessTeams, access.Teams)
		sort.Slice(sortedAccessTeams, func(i, j int) bool {
			return strings.ToLower(sortedAccessTeams[i].GetName()) < strings.ToLower(sortedAccessTeams[j].GetName())
		})

		for _, team := range sortedAccessTeams {
			sharedWith = append(sharedWith, team.GetName())
			// Mark all team members as covered
			visited := make(map[int64]bool)
			members := getAllMembers(team.GetID(), teamMembers, teamChildren, visited)
			for _, member := range members {
				coveredUsers[member.GetLogin()] = true
			}
		}

		// Add users not covered by any listed group
		var additionalUsers []string
		for _, collab := range access.Collaborators {
			if !coveredUsers[collab.GetLogin()] {
				additionalUsers = append(additionalUsers, collab.GetLogin())
			}
		}
		sort.Slice(additionalUsers, func(i, j int) bool {
			return strings.ToLower(additionalUsers[i]) < strings.ToLower(additionalUsers[j])
		})
		sharedWith = append(sharedWith, additionalUsers...)

		sharedWithStr := "-"
		if len(sharedWith) > 0 {
			sharedWithStr = strings.Join(sharedWith, ", ")
		}

		sb.WriteString(fmt.Sprintf("| %s | %s |\n", repoName, sharedWithStr))
	}

	sb.WriteString("\n")

	return sb.String()
}
