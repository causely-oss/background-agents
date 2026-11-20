package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// Names of the files written under the secrets directory. The gh wrapper
	// in the image (gh-wrapper.sh) reads ghTokenFile.
	ghTokenFile    = "gh-token"
	kubeconfigFile = "kubeconfig"
)

// runtimeEnv records which credentials the Actor was started with. Only
// presence is recorded; values never leave the process environment.
type runtimeEnv struct {
	AnthropicKey bool
	OpenAIKey    bool
	GitHubToken  bool
	// Kubectl is true when kubectl has some way to authenticate: a
	// kubeconfig from KUBECONFIG_DATA or KUBECONFIG, or an in-cluster
	// ServiceAccount token if Substrate mounts one.
	Kubectl bool
}

// prepareProcessEnvironment builds the Hermes process environment from the
// adapter's own, keeping credentials out of what Hermes' terminal tool
// passes to the commands the model runs:
//
//   - GH_TOKEN is written to secretsDir/gh-token and removed. Hermes refuses
//     to pass GH_TOKEN to subprocesses even when allowlisted, so the image's
//     gh wrapper reads the file instead (git uses gh as credential helper).
//   - KUBECONFIG_DATA is written to secretsDir/kubeconfig, removed, and
//     replaced by KUBECONFIG.
//   - Each kagent MCP header credential (headerSecrets) is renamed to
//     hermesSecretEnvName(name), which Hermes strips from every subprocess.
//     Under Substrate these hold only the egress placeholder, but a
//     standalone run may pass real values.
//   - kagent's own variables (KAGENT_TOKEN, the bootstrap descriptor and
//     the configuration) are removed.
//
// secretsDir must be outside /data, which Substrate snapshots.
func prepareProcessEnvironment(environ []string, secretsDir string, headerSecrets []string, fileExists func(string) bool) ([]string, runtimeEnv, error) {
	values := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		values[name] = value
	}
	present := func(name string) bool { return strings.TrimSpace(values[name]) != "" }
	env := runtimeEnv{
		AnthropicKey: present("ANTHROPIC_API_KEY"),
		OpenAIKey:    present("OPENAI_API_KEY"),
	}

	// KAGENT_TOKEN authenticates the Actor to the kagent control plane and
	// KAGENT_BOOTSTRAP_JSON locates its configuration; only the adapter uses
	// them, so the commands the model runs never see them.
	drop := []string{envGitHubToken, envKubeconfigData, "KAGENT_TOKEN", "KAGENT_BOOTSTRAP_JSON", configEnv, agentCardEnv}
	var add []string
	writeSecret := func(name, contents string) (string, error) {
		if err := os.MkdirAll(secretsDir, 0o700); err != nil {
			return "", fmt.Errorf("create secrets directory: %w", err)
		}
		path := filepath.Join(secretsDir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
		return path, nil
	}

	if present(envGitHubToken) {
		if _, err := writeSecret(ghTokenFile, strings.TrimSpace(values[envGitHubToken])); err != nil {
			return nil, runtimeEnv{}, err
		}
		env.GitHubToken = true
	}
	if present(envKubeconfigData) {
		path, err := writeSecret(kubeconfigFile, values[envKubeconfigData])
		if err != nil {
			return nil, runtimeEnv{}, err
		}
		drop = append(drop, "KUBECONFIG")
		add = append(add, "KUBECONFIG="+path)
		env.Kubectl = true
	} else if present(envKubeAPIServer) {
		path, err := writeSecret(kubeconfigFile, egressKubeconfig(strings.TrimSpace(values[envKubeAPIServer]), values["SSL_CERT_FILE"]))
		if err != nil {
			return nil, runtimeEnv{}, err
		}
		drop = append(drop, "KUBECONFIG")
		add = append(add, "KUBECONFIG="+path)
		env.Kubectl = true
	} else if present("KUBECONFIG") || fileExists(inClusterTokenPath) {
		env.Kubectl = true
	}
	for _, name := range headerSecrets {
		value, ok := values[name]
		if !ok {
			return nil, runtimeEnv{}, fmt.Errorf("MCP header credential %s is referenced by the kagent configuration but not set", name)
		}
		drop = append(drop, name)
		add = append(add, hermesSecretEnvName(name)+"="+value)
	}

	out := make([]string, 0, len(environ)+len(add))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(drop, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, add...), env, nil
}

// egressKubeconfig is a kubeconfig for an Actor whose Kubernetes credential
// lives in an EgressPolicy: kubectl sends a placeholder bearer token, which
// Substrate's egress gateway replaces with the ServiceAccount token, and it
// trusts the gateway's CA (SSL_CERT_FILE), because the gateway terminates TLS
// to add the header. Nothing in it is secret.
func egressKubeconfig(server, caFile string) string {
	cluster := "    server: " + server + "\n"
	if strings.TrimSpace(caFile) != "" {
		cluster += "    certificate-authority: " + caFile + "\n"
	}
	return "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: kubernetes\n  cluster:\n" + cluster +
		"users:\n- name: causely-investigator\n  user:\n    token: " + egressPlaceholder + "\n" +
		"contexts:\n- name: kubernetes\n  context:\n    cluster: kubernetes\n    user: causely-investigator\n" +
		"current-context: kubernetes\n"
}

// egressPlaceholder is the credential value tools send for a header that the
// egress gateway replaces (GH_TOKEN in harness.yaml uses the same value).
const egressPlaceholder = "egress-injected"

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
