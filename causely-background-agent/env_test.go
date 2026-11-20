package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func noFiles(string) bool { return false }

func readSecret(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode %v, want 0600", path, info.Mode())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestPrepareProcessEnvironmentMovesCredentialsOutOfEnv(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "secrets")
	environ := []string{
		"PATH=/bin",
		"KUBECONFIG=/old",
		"KUBECONFIG_DATA=apiVersion: v1\nkind: Config\n",
		"GH_TOKEN=github_pat_x\n",
		"ANTHROPIC_API_KEY=sk",
		"KAGENT_CREDENTIAL_ABC=Bearer causely",
		"KAGENT_TOKEN=actor-jwt",
		`KAGENT_BOOTSTRAP_JSON={"format_version":1}`,
	}
	out, env, err := prepareProcessEnvironment(environ, dir, []string{"KAGENT_CREDENTIAL_ABC"}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Kubectl || !env.GitHubToken || !env.AnthropicKey || env.OpenAIKey {
		t.Fatalf("runtimeEnv: %+v", env)
	}
	for _, kv := range out {
		for _, gone := range []string{"KUBECONFIG_DATA=", "KUBECONFIG=/old", "GH_TOKEN=", "KAGENT_CREDENTIAL_ABC=", "KAGENT_TOKEN=", "KAGENT_BOOTSTRAP_JSON="} {
			if strings.HasPrefix(kv, gone) {
				t.Fatalf("%q must not reach Hermes: %v", kv, out)
			}
		}
	}
	for _, want := range []string{
		"KUBECONFIG=" + filepath.Join(dir, kubeconfigFile),
		"AUXILIARY_KAGENT_CREDENTIAL_ABC_API_KEY=Bearer causely",
		"ANTHROPIC_API_KEY=sk", // Hermes needs it and strips it from subprocesses itself
		"PATH=/bin",
	} {
		if !slices.Contains(out, want) {
			t.Fatalf("missing %q in %v", want, out)
		}
	}
	if got := readSecret(t, filepath.Join(dir, kubeconfigFile)); got != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("kubeconfig %q", got)
	}
	if got := readSecret(t, filepath.Join(dir, ghTokenFile)); got != "github_pat_x" {
		t.Fatalf("gh token %q", got)
	}
}

func TestPrepareProcessEnvironmentMissingHeaderCredential(t *testing.T) {
	t.Parallel()
	_, _, err := prepareProcessEnvironment([]string{"PATH=/bin"}, t.TempDir(), []string{"KAGENT_CREDENTIAL_NOPE"}, noFiles)
	if err == nil || !strings.Contains(err.Error(), "KAGENT_CREDENTIAL_NOPE") {
		t.Fatalf("expected missing credential error, got %v", err)
	}
}

func TestPrepareProcessEnvironmentDetectsInCluster(t *testing.T) {
	t.Parallel()
	_, env, err := prepareProcessEnvironment([]string{"PATH=/bin"}, t.TempDir(), nil, func(p string) bool { return p == inClusterTokenPath })
	if err != nil || !env.Kubectl {
		t.Fatalf("expected in-cluster kubectl, got %+v %v", env, err)
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	_, env, err = prepareProcessEnvironment([]string{"PATH=/bin", "GH_TOKEN="}, dir, nil, noFiles)
	if err != nil || env.Kubectl || env.GitHubToken {
		t.Fatalf("expected no credentials, got %+v %v", env, err)
	}
	if fileExists(dir) {
		t.Fatal("no secrets directory should be created when there are no secrets")
	}
}

func TestPrepareProcessEnvironmentEgressKubeconfig(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "secrets")
	out, env, err := prepareProcessEnvironment([]string{
		"CAUSELY_KUBE_API_SERVER=https://kubernetes.default.svc",
		"SSL_CERT_FILE=/run/kagent/egress/trust-bundle.pem",
		"KUBECONFIG=/old",
	}, dir, nil, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Kubectl || !slices.Contains(out, "KUBECONFIG="+filepath.Join(dir, kubeconfigFile)) || slices.Contains(out, "KUBECONFIG=/old") {
		t.Fatalf("env %+v, out %v", env, out)
	}
	got := readSecret(t, filepath.Join(dir, kubeconfigFile))
	for _, want := range []string{
		"server: https://kubernetes.default.svc\n",
		"certificate-authority: /run/kagent/egress/trust-bundle.pem\n",
		"token: egress-injected\n",
		"current-context: kubernetes\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("kubeconfig missing %q:\n%s", want, got)
		}
	}
}
