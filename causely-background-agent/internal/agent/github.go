package agent

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

// fixBranchPrefix returns the stable branch-name prefix for a given issue,
// e.g. "causely-fix/32a3cbcc-15f2-4587-a015-06ed4470a7c5". Every fix attempt for
// the same issue shares this prefix, which is how findExistingFixPR locates
// prior attempts to update instead of duplicating.
func fixBranchPrefix(issueID string) string {
	var b strings.Builder
	for _, r := range issueID {
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
// this same issue, identified by its branch-name prefix. Without this,
// every trigger for the same issue_id opens a brand-new PR — a real recurring
// issue can fire this webhook multiple times before it's resolved.
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
// this issue is already open — pushes the new changes onto its existing
// branch and returns that PR's URL instead of opening a duplicate.
func (g *githubClient) CreatePR(fix ProposedFix, issueID string) (string, error) {
	branchPrefix := fixBranchPrefix(issueID)

	// Get the default branch name — needed by both the existing-PR path
	// (applyChangeIdempotent compares against its content) and the new-PR path.
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

	// A failure here is non-fatal: fall through and create a new PR rather than
	// blocking remediation entirely on a dedup-check failure. Worst case is the
	// pre-existing behavior (a duplicate PR), not a new failure mode.
	existingURL, existingBranch, _ := g.findExistingFixPR(branchPrefix)
	if existingURL != "" {
		if err := g.applyChangesIdempotent(existingBranch, repoInfo.DefaultBranch, fix.Changes); err != nil {
			return "", fmt.Errorf("apply changes on existing branch %s: %w", existingBranch, err)
		}
		return existingURL, nil
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

	// Create branch, named so future attempts at the same issue can find it.
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

// fetchFileContentAndSHA reads path's current content and blob SHA on branch.
func (g *githubClient) fetchFileContentAndSHA(branch, path string) (string, string, error) {
	raw, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", g.owner, g.repo, path, branch), nil)
	if err != nil {
		return "", "", err
	}
	var current struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	_ = json.Unmarshal(raw, &current)
	decoded, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(current.Content, "\n", ""))
	return string(decoded), current.SHA, nil
}

func (g *githubClient) commitFileContent(branch, path, content, sha string) error {
	_, err := g.do("PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", g.owner, g.repo, path), map[string]any{
		"message": "fix: " + path,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":     sha,
		"branch":  branch,
	})
	return err
}

// applyChange applies change to branch, used for a freshly-cut branch (just
// branched from the default branch, so change.Search is always expected to
// be present — no idempotency handling needed here, see applyChangeIdempotent
// for the existing-branch case).
func (g *githubClient) applyChange(branch string, change FileChange) error {
	current, sha, err := g.fetchFileContentAndSHA(branch, change.Path)
	if err != nil {
		return err
	}

	// Guard against a silent no-op commit: if change.Search isn't found verbatim in the
	// file's CURRENT content (as opposed to whatever Claude read earlier via read_file),
	// strings.Replace below would return the content unchanged and this would still PUT
	// it — a byte-identical commit with a "fix:" message and no actual diff. This is
	// exactly the failure mode found dogfooding: fixHasRealChange (agent.go) only checks
	// that Claude's *reported* search != replace, which says nothing about whether the
	// file has drifted since read_file was called (a concurrent edit, or simply that the
	// search text was never quite right). Fail loudly here instead of opening a fix PR
	// that fixes nothing.
	if !strings.Contains(current, change.Search) {
		return fmt.Errorf("search text not found in %s at its current content on branch %s — the file may have changed since it was read, or the proposed search string doesn't match verbatim; refusing to commit a no-op change", change.Path, branch)
	}
	newContent := strings.Replace(current, change.Search, change.Replace, 1)
	return g.commitFileContent(branch, change.Path, newContent, sha)
}

// applyChangeIdempotent behaves like applyChange, but is used for the
// existing-fix-PR path (see findExistingFixPR/CreatePR), where a prior
// investigation may have already committed this exact change onto branch —
// investigations always read the default branch, so a repeated proposal
// contains the ORIGINAL search text, which is legitimately gone from the
// existing branch once already replaced there.
//
// When change.Search isn't found on branch, this checks whether branch's
// content already equals what applying this change to baseBranch's content
// would produce — an exact-content comparison, not a bare
// strings.Contains(current, change.Replace) check, which is unsafe: a short
// or generic replacement (e.g. "true", "enabled", "}") can appear elsewhere
// in the file for unrelated reasons and produce a false "already applied,"
// silently skipping a change that was actually still required.
// applyChangesIdempotent applies changes to branch, grouping by Path first —
// a single proposal can contain multiple changes to the same file, and each
// must be applied against the result of the ones before it, not
// independently against the file's original content (which would make the
// second change's search text vanish before it's ever tried).
func (g *githubClient) applyChangesIdempotent(branch, baseBranch string, changes []FileChange) error {
	var order []string
	byPath := make(map[string][]FileChange, len(changes))
	for _, c := range changes {
		if _, ok := byPath[c.Path]; !ok {
			order = append(order, c.Path)
		}
		byPath[c.Path] = append(byPath[c.Path], c)
	}
	for _, path := range order {
		if err := g.applyFileChangesIdempotent(branch, baseBranch, path, byPath[path]); err != nil {
			return err
		}
	}
	return nil
}

// applyFileChangesIdempotent applies every change for one file, in order,
// to branch's current content. If the FIRST change's search text isn't
// found — the case findExistingFixPR's re-application onto an existing fix
// branch is meant to handle idempotently — it checks whether branch's
// content already equals what applying ALL of these changes, in order, to
// baseBranch's content would produce; if so, this file is fully already
// fixed and it's a no-op. Comparing the FULL expected content (not a bare
// substring check on any one change.Replace) avoids a false "already
// applied" from a short/generic replacement that happens to appear
// elsewhere in the file for unrelated reasons.
func (g *githubClient) applyFileChangesIdempotent(branch, baseBranch, path string, changes []FileChange) error {
	current, sha, err := g.fetchFileContentAndSHA(branch, path)
	if err != nil {
		return err
	}

	newContent := current
	for _, change := range changes {
		if !strings.Contains(newContent, change.Search) {
			baseContent, _, err := g.fetchFileContentAndSHA(baseBranch, path)
			if err != nil {
				return fmt.Errorf("fetch %s on base branch %s: %w", path, baseBranch, err)
			}
			expected, applyErr := applyFileChangesSequentially(baseContent, changes)
			// applyErr != nil means some change's search text was never
			// found even in base — silently skipping it (rather than
			// erroring) would let expected default to base content
			// unmodified, which can then coincidentally equal a genuinely
			// never-fixed branch and be reported as a false "already
			// applied" no-op. A change that was never validly applicable is
			// a real problem, not an idempotent success.
			if applyErr == nil && current == expected {
				return nil // already fully applied on this branch — idempotent no-op
			}
			if applyErr != nil {
				return fmt.Errorf("search text not found in %s at its current content on branch %s, and one of the proposed changes isn't applicable to the base branch %s either: %w — refusing to commit a no-op or invalid change", path, branch, baseBranch, applyErr)
			}
			return fmt.Errorf("search text not found in %s at its current content on branch %s, and its content doesn't match the expected result of applying all %d proposed change(s) to the base branch %s — the file may have changed since it was read, or a proposed search string doesn't match verbatim; refusing to commit a no-op or conflicting change", path, branch, len(changes), baseBranch)
		}
		newContent = strings.Replace(newContent, change.Search, change.Replace, 1)
	}
	if newContent == current {
		return nil // defensive: no actual change to commit
	}
	return g.commitFileContent(branch, path, newContent, sha)
}

// applyFileChangesSequentially applies each change's search/replace to
// content in order, REQUIRING every search to be found — unlike a variant
// that silently skips a missing one, which would let the result default to
// unmodified content for an invalid change and risk that coincidentally
// matching a genuinely never-fixed branch (a false "already applied").
func applyFileChangesSequentially(content string, changes []FileChange) (string, error) {
	for i, c := range changes {
		if !strings.Contains(content, c.Search) {
			return "", fmt.Errorf("change %d/%d: search text %q not found", i+1, len(changes), c.Search)
		}
		content = strings.Replace(content, c.Search, c.Replace, 1)
	}
	return content, nil
}
