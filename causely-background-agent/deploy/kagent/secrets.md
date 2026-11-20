# Secrets

Create these in the `kagent` namespace (where the Harness, AgentTemplate,
Agent, ModelConfig, RemoteMCPServer and EgressPolicy live) before applying
the manifests. Never commit their values.

| Secret | Key | Used by | Reaches |
|---|---|---|---|
| `causely-hermes-anthropic` | `api-key` | ModelConfig `causely-anthropic` | the Anthropic API, added by the egress gateway |
| `causely-hermes-causely-mcp` | `authorization` | RemoteMCPServer `causely` header | the Causely MCP server, added by the egress gateway |
| `causely-hermes-github` | `api-token` | EgressPolicy `causely-github` | `api.github.com`, as `Authorization: Bearer <token>` |
| `causely-hermes-github` | `git-basic` | EgressPolicy `causely-github` | `github.com` (git over HTTPS), as `Authorization: Basic <value>` |

```bash
kubectl -n kagent create secret generic causely-hermes-anthropic \
  --from-literal=api-key="$ANTHROPIC_API_KEY"

kubectl -n kagent create secret generic causely-hermes-causely-mcp \
  --from-literal=authorization="Bearer $CAUSELY_MCP_TOKEN"

kubectl -n kagent create secret generic causely-hermes-github \
  --from-literal=api-token="$GITHUB_TOKEN" \
  --from-literal=git-basic="$(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64)"
```

## How each credential is kept away from the model

With kagent 1.0 and Agent Substrate 1.0 no credential enters the Actor.
Substrate's egress gateway terminates TLS for the Actor's outgoing requests
(the Actor trusts it through `SSL_CERT_FILE`, `SSL_CERT_DIR` and
`GIT_SSL_CAINFO`, which Substrate sets) and replaces a credential header on
requests to the origins it is bound to:

- **Model key**: the Actor's `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is a
  placeholder (`kagent-credential-injected`). Hermes sends it; the gateway
  swaps in the ModelConfig's key on the way to the provider.
- **Causely MCP header**: kagent passes the header as
  `__KAGENT_ENV[KAGENT_CREDENTIAL_<hash>]__` and the variable holds a
  placeholder. The adapter still renames it to
  `AUXILIARY_KAGENT_CREDENTIAL_<hash>_API_KEY` (a name Hermes strips from all
  subprocesses) and writes the header as `${...}`, so a standalone run with a
  real value keeps it out of commands too.
- **GitHub token**: the Harness sets `GH_TOKEN=egress-injected`. The adapter
  writes it to `/tmp/causely-secrets/gh-token` for the `/usr/local/bin/gh`
  wrapper, git authenticates through `gh auth git-credential`, and both send
  the placeholder; the `causely-github` EgressPolicy replaces it.

The model can print its whole environment and learn only placeholders. What
it can do with the credentials through the gateway is still bounded only by
their scope:

- **GitHub**: a fine-grained personal access token or GitHub App token for the
  single repository in `CAUSELY_GITHUB_REPO`. Observe mode: Contents read,
  Metadata read, and Issues write for the investigation ledger (drop it if you
  set `CAUSELY_LEDGER=off`). Act mode: add Contents write and Pull requests
  write. Protect the default branch so a pushed branch can only become a PR.
  An observe-mode token without Contents or Pull requests write cannot push or
  open a PR however the model behaves; the worst it can do is file issues.
- **Causely**: a token for the tenant this deployment investigates. The
  AgentTemplate's tool allowlist hides Causely's write tools from the model,
  but the token itself is what bounds it.
- **Kubernetes**: see below.

## kubectl

kagent 1.0.1 Harness env takes literal values only, so a kubeconfig with a
token cannot be passed in from a Secret, and the shipped Harness runs without
kubectl. The adapter can use the same egress pattern: set
`CAUSELY_KUBE_API_SERVER` on the Harness to the API server's URL, and it
writes a kubeconfig with a placeholder bearer token and the gateway's CA. An
EgressPolicy then injects the token of the read-only ServiceAccount from
`rbac.yaml`:

```yaml
apiVersion: api.kagent.dev/v1alpha3
kind: EgressPolicy
metadata:
  name: causely-kubernetes
  namespace: kagent
spec:
  destinations:
    - origin: https://<api-server-host>
      authentication:
        header: Authorization
        prefix: "Bearer "
        secretKeyRef:
          name: causely-hermes-kubernetes   # key: token (the ServiceAccount token)
          key: token
```

This works only for an API server whose certificate the egress gateway
trusts. For the cluster kagent runs in (`kubernetes.default.svc.cluster.local`)
it does not: tested on kind, the gateway rejected the cluster CA
(`UnknownIssuer`), and an EgressPolicy has no field to add one. Actors also
resolve only full names (`*.svc.cluster.local`), not `kubernetes.default.svc`.
