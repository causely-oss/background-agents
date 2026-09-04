package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultGitHubAPIBaseURL = "https://api.github.com"

type githubClient struct {
	token   string
	owner   string
	repo    string
	client  *http.Client
	baseURL string // overridable in tests; defaults to defaultGitHubAPIBaseURL
}

func newGitHubClient(token, ownerRepo string) *githubClient {
	parts := strings.SplitN(ownerRepo, "/", 2)
	owner, repo := parts[0], ""
	if len(parts) == 2 {
		repo = parts[1]
	}
	return &githubClient{
		token:   token,
		owner:   owner,
		repo:    repo,
		client:  &http.Client{Timeout: 15 * time.Second},
		baseURL: defaultGitHubAPIBaseURL,
	}
}

func (g *githubClient) do(method, path string, body any) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.baseURL+path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github %s %s: %s — %s", method, path, resp.Status, string(raw))
	}
	return raw, nil
}

// ReadFile returns the decoded content of a file from the default branch.
func (g *githubClient) ReadFile(path string) (string, error) {
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s", g.owner, g.repo, path), nil)
	if err != nil {
		return "", err
	}
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", err
	}
	if f.Encoding != "base64" {
		return f.Content, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	return string(decoded), err
}

// ListDirectory returns a directory listing from the default branch.
func (g *githubClient) ListDirectory(path string) (string, error) {
	if path == "" || path == "." {
		path = ""
	}
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s", g.owner, g.repo, path), nil)
	if err != nil {
		return "", err
	}
	var items []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", err
	}
	var lines []string
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("%s  %s", item.Type, item.Path))
	}
	return strings.Join(lines, "\n"), nil
}

// SearchCode searches for code across the whole repository using GitHub's
// code search API, scoped to this client's repo. This gives the agent a way
// to jump straight to a relevant file by symbol/string instead of having to
// walk the directory tree blindly — important for navigating a large
// monorepo within the iteration/cost budget.
func (g *githubClient) SearchCode(query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("search query must not be empty")
	}
	scoped := fmt.Sprintf("%s repo:%s/%s", query, g.owner, g.repo)
	raw, err := g.do("GET", "/search/code?per_page=20&q="+url.QueryEscape(scoped), nil)
	if err != nil {
		return "", err
	}
	var res struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Path       string `json:"path"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode search results: %w", err)
	}
	if len(res.Items) == 0 {
		return "no matches found", nil
	}
	var lines []string
	for _, item := range res.Items {
		lines = append(lines, item.Path)
	}
	return fmt.Sprintf("%d total matches, showing %d:\n%s", res.TotalCount, len(lines), strings.Join(lines, "\n")), nil
}

// fixBranchPrefix returns the stable branch-name prefix for a given root cause,
// e.g. "causely-fix/32a3cbcc-15f2-4587-a015-06ed4470a7c5". Every fix attempt for
// the same root cause shares this prefix, which is how findExistingFixPR locates
// prior attempts to update instead of duplicating.
func fixBranchPrefix(rootCauseID string) string {
	var b strings.Builder
	for _, r := range rootCauseID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	id := b.String()
	if id == "" {
		id = "unknown"
	}
	return "causely-fix/" + id
}

// findExistingFixPR looks for an already-open PR from a prior attempt at fixing
// this same root cause, identified by its branch-name prefix. Without this,
// every trigger for the same root_cause_id opens a brand-new PR — a real root
// cause can fire this webhook multiple times before it's resolved.
func (g *githubClient) findExistingFixPR(branchPrefix string) (prURL string, branch string, err error) {
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100", g.owner, g.repo), nil)
	if err != nil {
		return "", "", fmt.Errorf("list open PRs: %w", err)
	}
	var pulls []struct {
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return "", "", fmt.Errorf("decode PR list: %w", err)
	}
	for _, pr := range pulls {
		if strings.HasPrefix(pr.Head.Ref, branchPrefix+"-") {
			return pr.HTMLURL, pr.Head.Ref, nil
		}
	}
	return "", "", nil
}

// CreatePR applies the proposed changes and opens a GitHub PR, or — if a PR for
// this root cause is already open — pushes the new changes onto its existing
// branch and returns that PR's URL instead of opening a duplicate.
func (g *githubClient) CreatePR(fix ProposedFix, rootCauseID string) (string, error) {
	branchPrefix := fixBranchPrefix(rootCauseID)

	// A failure here is non-fatal: fall through and create a new PR rather than
	// blocking remediation entirely on a dedup-check failure. Worst case is the
	// pre-existing behavior (a duplicate PR), not a new failure mode.
	existingURL, existingBranch, _ := g.findExistingFixPR(branchPrefix)
	if existingURL != "" {
		for _, change := range fix.Changes {
			if err := g.applyChange(existingBranch, change); err != nil {
				return "", fmt.Errorf("apply change to %s on existing branch %s: %w", change.Path, existingBranch, err)
			}
		}
		return existingURL, nil
	}

	// Get default branch SHA.
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", g.owner, g.repo), nil)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	_ = json.Unmarshal(raw, &repoInfo)
	if repoInfo.DefaultBranch == "" {
		repoInfo.DefaultBranch = "main"
	}

	raw, err = g.do("GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", g.owner, g.repo, repoInfo.DefaultBranch), nil)
	if err != nil {
		return "", fmt.Errorf("get ref: %w", err)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_ = json.Unmarshal(raw, &ref)

	// Create branch, named so future attempts at the same root cause can find it.
	branch := fmt.Sprintf("%s-%d", branchPrefix, time.Now().Unix())
	_, err = g.do("POST", fmt.Sprintf("/repos/%s/%s/git/refs", g.owner, g.repo), map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": ref.Object.SHA,
	})
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	// Apply each file change.
	for _, change := range fix.Changes {
		if err := g.applyChange(branch, change); err != nil {
			return "", fmt.Errorf("apply change to %s: %w", change.Path, err)
		}
	}

	// Open PR.
	raw, err = g.do("POST", fmt.Sprintf("/repos/%s/%s/pulls", g.owner, g.repo), map[string]any{
		"title": fix.PRTitle,
		"body":  fix.PRBody,
		"head":  branch,
		"base":  repoInfo.DefaultBranch,
	})
	if err != nil {
		return "", fmt.Errorf("create PR: %w", err)
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(raw, &pr)
	return pr.HTMLURL, nil
}

func (g *githubClient) applyChange(branch string, change FileChange) error {
	// Read current file content and SHA.
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", g.owner, g.repo, change.Path, branch), nil)
	if err != nil {
		return err
	}
	var current struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	_ = json.Unmarshal(raw, &current)
	decoded, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(current.Content, "\n", ""))

	// Guard against a silent no-op commit: if change.Search isn't found verbatim in the
	// file's CURRENT content (as opposed to whatever Claude read earlier via read_file),
	// strings.Replace below would return the content unchanged and this would still PUT
	// it — a byte-identical commit with a "fix:" message and no actual diff. This is
	// exactly the failure mode found dogfooding: fixHasRealChange (agent.go) only checks
	// that Claude's *reported* search != replace, which says nothing about whether the
	// file has drifted since read_file was called (a concurrent edit, or simply that the
	// search text was never quite right). Fail loudly here instead of opening a fix PR
	// that fixes nothing.
	if !strings.Contains(string(decoded), change.Search) {
		return fmt.Errorf("search text not found in %s at its current content on branch %s — the file may have changed since it was read, or the proposed search string doesn't match verbatim; refusing to commit a no-op change", change.Path, branch)
	}
	newContent := strings.Replace(string(decoded), change.Search, change.Replace, 1)

	// Commit the change.
	_, err = g.do("PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", g.owner, g.repo, change.Path), map[string]any{
		"message": "fix: " + change.Path,
		"content": base64.StdEncoding.EncodeToString([]byte(newContent)),
		"sha":     current.SHA,
		"branch":  branch,
	})
	return err
}
