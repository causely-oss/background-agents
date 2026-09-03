package main

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
		name        string
		rootCauseID string
		want        string
	}{
		{
			name:        "typical UUID root cause id",
			rootCauseID: "32a3cbcc-15f2-4587-a015-06ed4470a7c5",
			want:        "causely-fix/32a3cbcc-15f2-4587-a015-06ed4470a7c5",
		},
		{
			name:        "characters unsafe for a branch name are sanitized",
			rootCauseID: "rc/with spaces:and*stars",
			want:        "causely-fix/rc-with-spaces-and-stars",
		},
		{
			name:        "empty root cause id falls back to a stable placeholder",
			rootCauseID: "",
			want:        "causely-fix/unknown",
		},
		{
			name:        "same id always produces the same prefix (determinism, required for dedup to work)",
			rootCauseID: "abc-123",
			want:        "causely-fix/abc-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixBranchPrefix(tt.rootCauseID)
			if got != tt.want {
				t.Errorf("fixBranchPrefix(%q) = %q, want %q", tt.rootCauseID, got, tt.want)
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
