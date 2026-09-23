package agent

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixBranchPrefix(t *testing.T) {
	tests := []struct {
		name    string
		issueID string
		want    string
	}{
		{
			name:    "typical UUID issue id",
			issueID: "32a3cbcc-15f2-4587-a015-06ed4470a7c5",
			want:    "causely-fix/32a3cbcc-15f2-4587-a015-06ed4470a7c5",
		},
		{
			name:    "characters unsafe for a branch name are sanitized",
			issueID: "rc/with spaces:and*stars",
			want:    "causely-fix/rc-with-spaces-and-stars",
		},
		{
			name:    "empty issue id falls back to a stable placeholder",
			issueID: "",
			want:    "causely-fix/unknown",
		},
		{
			name:    "same id always produces the same prefix (determinism, required for dedup to work)",
			issueID: "abc-123",
			want:    "causely-fix/abc-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixBranchPrefix(tt.issueID)
			if got != tt.want {
				t.Errorf("fixBranchPrefix(%q) = %q, want %q", tt.issueID, got, tt.want)
			}
		})
	}
}

// fakeGitHub is a minimal in-memory GitHub API double covering just the
// endpoints CreatePR/applyChange/findExistingFixPR exercise.
type fakeGitHub struct {
	openPulls []struct {
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	fileContent   map[string]string // branch -> current file content
	createdBranch string
	createdPR     bool
	commits       []string // branch names that received a content PUT
}

func newFakeGitHubServer(t *testing.T, state *fakeGitHub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(state.openPulls)
	})
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	})
	mux.HandleFunc("GET /repos/org/repo/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "deadbeef"}})
	})
	mux.HandleFunc("POST /repos/org/repo/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ref string `json:"ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.createdBranch = strings.TrimPrefix(body.Ref, "refs/heads/")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /repos/org/repo/contents/app.py", func(w http.ResponseWriter, r *http.Request) {
		branch := r.URL.Query().Get("ref")
		content, ok := state.fileContent[branch]
		if !ok {
			content = "original content"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			"encoding": "base64",
			"sha":      "filesha",
		})
	})
	mux.HandleFunc("PUT /repos/org/repo/contents/app.py", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Branch string `json:"branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.commits = append(state.commits, body.Branch)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		state.createdPR = true
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/org/repo/pull/99"})
	})

	return httptest.NewServer(mux)
}

func newTestGitHubClient(baseURL string) *githubClient {
	c := newGitHubClient("test-token", "org/repo")
	c.baseURL = baseURL
	return c
}

func TestCreatePR_NoExistingPR_OpensNewOne(t *testing.T) {
	state := &fakeGitHub{fileContent: map[string]string{}}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: typo",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "original", Replace: "fixed"}},
	}

	url, err := client.CreatePR(fix, "rc-123")
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if url != "https://github.com/org/repo/pull/99" {
		t.Errorf("CreatePR() url = %q, want the newly created PR URL", url)
	}
	if !state.createdPR {
		t.Error("expected a new PR to be created")
	}
	if !strings.HasPrefix(state.createdBranch, "causely-fix/rc-123-") {
		t.Errorf("created branch = %q, want prefix causely-fix/rc-123-", state.createdBranch)
	}
}

// TestCreatePR_SearchNotFoundInCurrentContent guards against the silent no-op
// commit found dogfooding: fixHasRealChange (agent.go) only checks Claude's
// *reported* search != replace, which says nothing about whether the file
// actually contains that search text right now — e.g. it changed since
// read_file was called, or the string never matched verbatim. applyChange
// must refuse to commit rather than silently PUT the file back unchanged.
func TestCreatePR_SearchNotFoundInCurrentContent(t *testing.T) {
	// newFakeGitHubServer's contents handler defaults to "original content" for
	// any branch not explicitly seeded — exactly the case here, since CreatePR
	// creates a fresh, timestamp-suffixed branch name we can't predict.
	state := &fakeGitHub{fileContent: map[string]string{}}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: typo",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "text that does not appear in the file", Replace: "fixed"}},
	}

	_, err := client.CreatePR(fix, "rc-123")
	if err == nil {
		t.Fatal("CreatePR() expected an error when the search text isn't found in the current file content, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to explain the search text wasn't found", err)
	}
	if len(state.commits) != 0 {
		t.Error("expected no commit (PUT) to have been made")
	}
}

func TestCreatePR_ExistingOpenPR_UpdatesInsteadOfDuplicating(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		fileContent: map[string]string{},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: typo v2",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "original", Replace: "fixed"}},
	}

	url, err := client.CreatePR(fix, "rc-123")
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if url != "https://github.com/org/repo/pull/5" {
		t.Errorf("CreatePR() url = %q, want the existing PR URL (dedup)", url)
	}
	if state.createdPR {
		t.Error("expected no new PR to be created — this is the case #1/#2/#4-style duplicate-PR bug this test guards against")
	}
	if state.createdBranch != "" {
		t.Error("expected no new branch to be created")
	}
	if len(state.commits) != 1 || state.commits[0] != existingBranch {
		t.Errorf("expected one commit onto the existing branch %q, got commits=%v", existingBranch, state.commits)
	}
}

// TestCreatePR_ExistingOpenPR_AlreadyAppliedChangeIsIdempotent guards against
// case #4: a repeat investigation reads the default branch (still has the
// original text) and proposes the same search/replace, but the existing PR
// branch already has that exact replacement committed from a prior run — so
// change.Search is legitimately absent there, not because of drift. This
// must be treated as "already fixed," not fail the whole investigation.
func TestCreatePR_ExistingOpenPR_AlreadyAppliedChangeIsIdempotent(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		// The existing branch already has "fixed" (change.Replace) committed —
		// "original" (change.Search) is gone because it was already replaced.
		fileContent: map[string]string{existingBranch: "fixed content"},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: typo v2",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "original", Replace: "fixed"}},
	}

	url, err := client.CreatePR(fix, "rc-123")
	if err != nil {
		t.Fatalf("CreatePR() error = %v, want nil — an already-applied change should be treated as idempotent", err)
	}
	if url != "https://github.com/org/repo/pull/5" {
		t.Errorf("CreatePR() url = %q, want the existing PR URL", url)
	}
	if len(state.commits) != 0 {
		t.Errorf("expected no commit — the change is already present, want a no-op, got commits=%v", state.commits)
	}
}

// TestCreatePR_ExistingOpenPR_SearchMissingFromBaseAndExistingIsAnError
// guards the fix for the silent-skip bug: applyFileChangesSequentially used
// to skip (not error on) a change whose search text wasn't found even in the
// base branch, letting "expected" default to unmodified base content. If the
// existing branch ALSO happens to equal that same unmodified content (a
// genuinely never-applied, invalid proposal), the old code would compare
// current == expected, find them equal, and falsely report "already
// applied" — silently leaving the real problem unfixed. Both base and the
// existing branch here lack the search text entirely; this must be a real
// error, not a false no-op success.
func TestCreatePR_ExistingOpenPR_SearchMissingFromBaseAndExistingIsAnError(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	const untouchedContent = "unrelated_setting = 1\n"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		fileContent: map[string]string{
			// "main" is the fake server's hardcoded default branch. Both it
			// and the existing branch are identical, untouched content —
			// neither has ever seen this change applied, because the search
			// text was never valid against this file at all.
			"main":         untouchedContent,
			existingBranch: untouchedContent,
		},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: bogus setting",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "target_setting = false", Replace: "target_setting = true"}},
	}

	_, err := client.CreatePR(fix, "rc-123")
	if err == nil {
		t.Fatal("CreatePR() should error — the search text was never valid against base OR the existing branch; this must not be reported as a false idempotent no-op")
	}
	if len(state.commits) != 0 {
		t.Errorf("expected no commit on a genuinely invalid change, got commits=%v", state.commits)
	}
}

// TestCreatePR_ExistingOpenPR_GenericReplacementTextDoesNotFalsePositive
// guards against a bare strings.Contains(current, change.Replace) check: a
// short/generic replacement like "true" can legitimately appear elsewhere in
// the file for unrelated reasons even though THIS specific change was never
// actually applied. applyChangeIdempotent must compare full expected content,
// not just check whether the replacement substring exists anywhere.
func TestCreatePR_ExistingOpenPR_GenericReplacementTextDoesNotFalsePositive(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		// "true" appears in the file for an unrelated reason (some other
		// flag), but the specific change this fix proposes (replacing
		// "enable_x = false" with "enable_x = true") was never actually
		// applied — the search text isn't present because it never matched
		// verbatim in the first place, not because it was already replaced.
		fileContent: map[string]string{existingBranch: "unrelated_flag = true\nenable_x = maybe"},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: enable x",
		PRBody:  "body",
		Changes: []FileChange{{Path: "app.py", Search: "enable_x = false", Replace: "enable_x = true"}},
	}

	_, err := client.CreatePR(fix, "rc-123")
	if err == nil {
		t.Fatal("CreatePR() should error — the change was never actually applied, a bare substring match on \"true\" must not be treated as idempotent")
	}
	if len(state.commits) != 0 {
		t.Errorf("expected no commit on a genuine mismatch, got commits=%v", state.commits)
	}
}

// TestCreatePR_ExistingOpenPR_TwoChangesToSameFileIsIdempotent guards case
// #3: a single proposal with two changes to the SAME file, both already
// applied (from a prior run) on the existing branch. Applying each change
// independently against the file's current content would make the second
// change's search text vanish before it's ever reached (since the first
// change already transformed the file) — this must still be recognized as
// "already fully applied," not fail partway through.
func TestCreatePR_ExistingOpenPR_TwoChangesToSameFileIsIdempotent(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		fileContent: map[string]string{
			// "main" is the fake server's hardcoded default branch.
			"main":         "timeout = 10\nretries = 3\n",
			existingBranch: "timeout = 30\nretries = 5\n", // both changes already applied here
		},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: retry tuning",
		PRBody:  "body",
		Changes: []FileChange{
			{Path: "app.py", Search: "timeout = 10", Replace: "timeout = 30"},
			{Path: "app.py", Search: "retries = 3", Replace: "retries = 5"},
		},
	}

	url, err := client.CreatePR(fix, "rc-123")
	if err != nil {
		t.Fatalf("CreatePR() error = %v, want nil — both changes are already applied, this should be an idempotent no-op", err)
	}
	if url != "https://github.com/org/repo/pull/5" {
		t.Errorf("CreatePR() url = %q, want the existing PR URL", url)
	}
	if len(state.commits) != 0 {
		t.Errorf("expected no commit — both changes already present, got commits=%v", state.commits)
	}
}

// TestCreatePR_ExistingOpenPR_TwoChangesToSameFileAppliesBoth guards the
// normal (non-idempotent) case for the same grouping code path: neither
// change has been applied yet, both must be applied in one commit.
func TestCreatePR_ExistingOpenPR_TwoChangesToSameFileAppliesBoth(t *testing.T) {
	existingBranch := "causely-fix/rc-123-1700000000"
	state := &fakeGitHub{
		openPulls: []struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{
			{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
				Ref string `json:"ref"`
			}{Ref: existingBranch}},
		},
		fileContent: map[string]string{
			existingBranch: "timeout = 10\nretries = 3\n",
		},
	}
	server := newFakeGitHubServer(t, state)
	defer server.Close()

	client := newTestGitHubClient(server.URL)
	fix := ProposedFix{
		PRTitle: "fix: retry tuning",
		PRBody:  "body",
		Changes: []FileChange{
			{Path: "app.py", Search: "timeout = 10", Replace: "timeout = 30"},
			{Path: "app.py", Search: "retries = 3", Replace: "retries = 5"},
		},
	}

	_, err := client.CreatePR(fix, "rc-123")
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if len(state.commits) != 1 || state.commits[0] != existingBranch {
		t.Fatalf("expected exactly one commit onto %q, got commits=%v", existingBranch, state.commits)
	}
}

func TestSearchCode(t *testing.T) {
	t.Run("returns matching paths", func(t *testing.T) {
		mux := http.NewServeMux()
		var gotQuery string
		mux.HandleFunc("GET /search/code", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query().Get("q")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 2,
				"items": []map[string]any{
					{"path": "cmd/foo/main.go"},
					{"path": "pkg/bar/baz.go"},
				},
			})
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		client := newTestGitHubClient(server.URL)
		result, err := client.SearchCode("DATBASE_URL")
		if err != nil {
			t.Fatalf("SearchCode() error = %v", err)
		}
		if !strings.Contains(result, "cmd/foo/main.go") || !strings.Contains(result, "pkg/bar/baz.go") {
			t.Errorf("SearchCode() = %q, want both matching paths listed", result)
		}
		if !strings.Contains(gotQuery, "repo:org/repo") {
			t.Errorf("query %q was not scoped to repo:org/repo", gotQuery)
		}
	})

	t.Run("no matches is not an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /search/code", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []map[string]any{}})
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		client := newTestGitHubClient(server.URL)
		result, err := client.SearchCode("nonexistent_symbol_xyz")
		if err != nil {
			t.Fatalf("SearchCode() error = %v", err)
		}
		if !strings.Contains(result, "no matches") {
			t.Errorf("SearchCode() = %q, want a no-matches message", result)
		}
	})

	t.Run("empty query is rejected before hitting the API", func(t *testing.T) {
		client := newTestGitHubClient("http://unused.invalid")
		if _, err := client.SearchCode("   "); err == nil {
			t.Error("expected an error for an empty/whitespace query")
		}
	})
}

func TestFindExistingFixPR(t *testing.T) {
	t.Run("finds a matching open PR", func(t *testing.T) {
		state := &fakeGitHub{
			openPulls: []struct {
				HTMLURL string `json:"html_url"`
				Head    struct {
					Ref string `json:"ref"`
				} `json:"head"`
			}{
				{HTMLURL: "https://github.com/org/repo/pull/1", Head: struct {
					Ref string `json:"ref"`
				}{Ref: "some-other-branch"}},
				{HTMLURL: "https://github.com/org/repo/pull/5", Head: struct {
					Ref string `json:"ref"`
				}{Ref: "causely-fix/rc-123-1700000000"}},
			},
		}
		server := newFakeGitHubServer(t, state)
		defer server.Close()

		client := newTestGitHubClient(server.URL)
		url, branch, err := client.findExistingFixPR("causely-fix/rc-123")
		if err != nil {
			t.Fatalf("findExistingFixPR() error = %v", err)
		}
		if url != "https://github.com/org/repo/pull/5" || branch != "causely-fix/rc-123-1700000000" {
			t.Errorf("findExistingFixPR() = (%q, %q), want pull/5 on the matching branch", url, branch)
		}
	})

	t.Run("no match returns empty, not an error", func(t *testing.T) {
		state := &fakeGitHub{}
		server := newFakeGitHubServer(t, state)
		defer server.Close()

		client := newTestGitHubClient(server.URL)
		url, branch, err := client.findExistingFixPR("causely-fix/rc-999")
		if err != nil {
			t.Fatalf("findExistingFixPR() error = %v", err)
		}
		if url != "" || branch != "" {
			t.Errorf("findExistingFixPR() = (%q, %q), want empty", url, branch)
		}
	})
}
