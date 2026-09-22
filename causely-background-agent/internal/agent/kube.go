package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// kubeClient wraps a Kubernetes clientset. It exists (non-nil) only when this
// pod is actually running in a cluster AND has at least one real RBAC grant —
// see newKubeClient. Deliberately does NOT expose Secret values:
// kubectl_get_secret_keys returns key names only, since Secret content
// flowing into Claude's context risks it being echoed back into a PR body,
// Slack message, or the investigation record.
type kubeClient struct {
	clientset kubernetes.Interface
	perms     kubePermissions
}

// kindResource pairs a kubectl-style kind name with the (apiGroup, resource)
// SelfSubjectAccessReview needs to check RBAC for it.
type kindResource struct{ group, resource string }

// readableKindResources/restartableKindResources/scalableKindResources are
// the kinds kubectl_get/kubectl_rollout_restart/kubectl_scale each support —
// checked individually, not as one coarse "workloads" bucket, so buildTools
// can advertise exactly the kinds this deployment's RBAC actually grants,
// not all of them just because ONE of them is granted.
var readableKindResources = map[string]kindResource{
	"deployment":  {"apps", "deployments"},
	"statefulset": {"apps", "statefulsets"},
	"daemonset":   {"apps", "daemonsets"},
	"replicaset":  {"apps", "replicasets"},
	"pod":         {"", "pods"},
	"service":     {"", "services"},
	"configmap":   {"", "configmaps"},
}

var restartableKindResources = map[string]kindResource{
	"deployment":  {"apps", "deployments"},
	"statefulset": {"apps", "statefulsets"},
	"daemonset":   {"apps", "daemonsets"},
}

var scalableKindResources = map[string]kindResource{
	"deployment":  {"apps", "deployments"},
	"statefulset": {"apps", "statefulsets"},
	"replicaset":  {"apps", "replicasets"},
}

// kubePermissions records what this ServiceAccount is actually authorized to
// do, checked once at startup via SelfSubjectAccessReview — not merely
// whether in-cluster credentials exist (every pod has *a* ServiceAccount
// token; that says nothing about what deploy/rbac.yaml actually granted it),
// and not as coarse per-verb buckets either — buildTools uses the per-kind
// maps to advertise exactly the kinds each tool will actually work for,
// instead of offering e.g. kubectl_get claiming support for all 7 kinds when
// only some of them are actually readable.
type kubePermissions struct {
	readableKinds    map[string]bool // kind -> get granted
	restartableKinds map[string]bool // kind -> patch granted (rollout restart)
	scalableKinds    map[string]bool // kind -> update granted (scale)
	canReadPodLogs   bool
	canReadSecrets   bool // opt-in, see deploy/rbac-secrets.example.yaml
}

func (p kubePermissions) canRead() bool {
	return len(p.readableKindsList()) > 0 || p.canReadPodLogs || p.canReadSecrets
}

func (p kubePermissions) readableKindsList() []string    { return sortedTrueKeys(p.readableKinds) }
func (p kubePermissions) restartableKindsList() []string { return sortedTrueKeys(p.restartableKinds) }
func (p kubePermissions) scalableKindsList() []string    { return sortedTrueKeys(p.scalableKinds) }

func sortedTrueKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// checkAccess asks the API server whether this ServiceAccount can perform
// verb on resource in namespace (empty namespace means cluster-scoped), via
// SelfSubjectAccessReview — the only reliable way to know what RBAC actually
// granted, short of trying the call and seeing if it 403s. A review failure
// (e.g. the SelfSubjectAccessReview API itself is blocked) is treated as "not
// allowed" — fail closed.
func checkAccess(clientset kubernetes.Interface, group, resource, verb, namespace string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:     group,
				Resource:  resource,
				Verb:      verb,
				Namespace: namespace,
			},
		},
	}
	result, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false
	}
	return result.Status.Allowed
}

// checkAccessAnyNamespace reports whether verb on resource is granted either
// cluster-wide, or in at least one of namespaces. deploy/rbac.yaml grants
// mutate/secrets access via namespaced RoleBindings on purpose (see its own
// comments) — checking only the cluster-scoped case (an empty namespace)
// would report false for exactly the access pattern the RBAC manifest is
// designed around, never offering those tools even when correctly granted.
func checkAccessAnyNamespace(clientset kubernetes.Interface, group, resource, verb string, namespaces []string) bool {
	if checkAccess(clientset, group, resource, verb, "") {
		return true
	}
	for _, ns := range namespaces {
		if checkAccess(clientset, group, resource, verb, ns) {
			return true
		}
	}
	return false
}

// newKubeClient attempts in-cluster auto-detection (the standard
// ServiceAccount token + CA cert every pod gets mounted automatically), then
// checks what deploy/rbac.yaml actually granted it — scopeNamespaces should
// be cfg.ScopeNamespaces, since that's what the mutate/secrets RoleBindings
// are bound against. It returns (nil, err) rather than a fatal error when
// unavailable — running locally outside a cluster, or a deployment that
// hasn't applied deploy/rbac.yaml at all — so the kubectl tools are simply
// omitted rather than blocking startup, matching how an unreachable MCP
// server degrades in loadMCPSources.
func newKubeClient(scopeNamespaces []string) (*kubeClient, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("not running in-cluster: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}

	perms := kubePermissions{
		readableKinds:    make(map[string]bool, len(readableKindResources)),
		restartableKinds: make(map[string]bool, len(restartableKindResources)),
		scalableKinds:    make(map[string]bool, len(scalableKindResources)),
		canReadPodLogs:   checkAccess(clientset, "", "pods/log", "get", ""),
		// Deliberately checked cluster-wide OR per scopeNamespaces:
		// deploy/rbac-secrets.example.yaml grants it via a namespaced
		// RoleBinding by design, so checking only the cluster-scoped case
		// would always report false for exactly the access pattern it grants.
		canReadSecrets: checkAccessAnyNamespace(clientset, "", "secrets", "get", scopeNamespaces),
	}
	// Read is checked cluster-wide only, matching deploy/rbac.yaml's read
	// ClusterRole, which is deliberately bound via ClusterRoleBinding, not
	// per-namespace (see its own comment on why diagnosis shouldn't be
	// namespace-blind). Checked per KIND, not as one coarse bucket, so a
	// partial grant (e.g. pods but not deployments) is reflected accurately.
	for kind, gr := range readableKindResources {
		perms.readableKinds[kind] = checkAccess(clientset, gr.group, gr.resource, "get", "")
	}
	// Mutate is checked cluster-wide OR per scopeNamespaces, matching how
	// deploy/rbac.yaml's mutate ClusterRole is bound via a namespaced
	// RoleBinding by design.
	for kind, gr := range restartableKindResources {
		perms.restartableKinds[kind] = checkAccessAnyNamespace(clientset, gr.group, gr.resource, "patch", scopeNamespaces)
	}
	for kind, gr := range scalableKindResources {
		perms.scalableKinds[kind] = checkAccessAnyNamespace(clientset, gr.group, gr.resource, "update", scopeNamespaces)
	}
	if !perms.canRead() {
		return nil, fmt.Errorf("in-cluster credentials present but deploy/rbac.yaml's read ClusterRole isn't granted to this ServiceAccount")
	}

	return &kubeClient{clientset: clientset, perms: perms}, nil
}

// readableKinds is the allowlist for kubectl_get — deliberately excludes
// Secret (see kubeClient doc comment); use kubectl_get_secret_keys instead.
var readableKinds = map[string]bool{
	"deployment":  true,
	"statefulset": true,
	"daemonset":   true,
	"replicaset":  true,
	"pod":         true,
	"service":     true,
	"configmap":   true,
}

// GetResource fetches one resource as indented JSON. kind is matched
// case-insensitively and accepts common plurals (e.g. "deployments").
func (k *kubeClient) GetResource(ctx context.Context, kind, namespace, name string) (string, error) {
	kind = normalizeKind(kind)
	if kind == "secret" {
		return "", fmt.Errorf("kubectl_get does not expose Secret content — use kubectl_get_secret_keys to see what keys a Secret has, without its values")
	}
	if !readableKinds[kind] {
		return "", fmt.Errorf("unsupported kind %q — supported: deployment, statefulset, daemonset, replicaset, pod, service, configmap", kind)
	}

	var obj any
	var err error
	switch kind {
	case "deployment":
		obj, err = k.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	case "statefulset":
		obj, err = k.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	case "daemonset":
		obj, err = k.clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	case "replicaset":
		obj, err = k.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
	case "pod":
		obj, err = k.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	case "service":
		obj, err = k.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	case "configmap":
		obj, err = k.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", describeKubeError(err, kind, namespace, name)
	}
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", kind, err)
	}
	return string(raw), nil
}

// GetSecretKeys returns the sorted key names of a Secret's data map — never
// the decoded values. Lets Claude confirm a Secret's shape (e.g. "does
// otel-collector-conf have a key named otel-collector-config.yaml") without
// its content ever entering the conversation.
func (k *kubeClient) GetSecretKeys(ctx context.Context, namespace, name string) (string, error) {
	secret, err := k.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", describeKubeError(err, "secret", namespace, name)
	}
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	raw, _ := json.Marshal(keys)
	return string(raw), nil
}

// GetPodLogs returns up to tailLines of a pod's log output. container may be
// empty if the pod has only one container.
func (k *kubeClient) GetPodLogs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{TailLines: &tailLines}
	if container != "" {
		opts.Container = container
	}
	raw, err := k.clientset.CoreV1().Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", describeKubeError(err, "pod logs", namespace, pod)
	}
	return string(raw), nil
}

// restartableKinds mirrors what `kubectl rollout restart` supports.
var restartableKinds = map[string]bool{"deployment": true, "statefulset": true, "daemonset": true}

// RolloutRestart patches the pod template's restartedAt annotation — exactly
// what `kubectl rollout restart` does under the hood — triggering a rolling
// restart. Only ever called when the caller has already confirmed act mode;
// this method itself performs no such check, so callers must gate it.
func (k *kubeClient) RolloutRestart(ctx context.Context, kind, namespace, name string) (string, error) {
	kind = normalizeKind(kind)
	if !restartableKinds[kind] {
		return "", fmt.Errorf("unsupported kind %q for rollout restart — supported: deployment, statefulset, daemonset", kind)
	}
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339)))

	var err error
	switch kind {
	case "deployment":
		_, err = k.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, err = k.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "daemonset":
		_, err = k.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return "", describeKubeError(err, kind, namespace, name)
	}
	return fmt.Sprintf("restarted %s %s/%s", kind, namespace, name), nil
}

// scalableKinds mirrors what `kubectl scale` supports.
var scalableKinds = map[string]bool{"deployment": true, "statefulset": true, "replicaset": true}

// ScaleResource sets replica count. Only ever called when the caller has
// already confirmed act mode; this method itself performs no such check.
func (k *kubeClient) ScaleResource(ctx context.Context, kind, namespace, name string, replicas int32) (string, error) {
	kind = normalizeKind(kind)
	if !scalableKinds[kind] {
		return "", fmt.Errorf("unsupported kind %q for scale — supported: deployment, statefulset, replicaset", kind)
	}
	if replicas < 0 {
		return "", fmt.Errorf("replicas must be >= 0, got %d", replicas)
	}

	var err error
	switch kind {
	case "deployment":
		var d *appsv1.Deployment
		d, err = k.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			d.Spec.Replicas = &replicas
			_, err = k.clientset.AppsV1().Deployments(namespace).Update(ctx, d, metav1.UpdateOptions{})
		}
	case "statefulset":
		var s *appsv1.StatefulSet
		s, err = k.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			s.Spec.Replicas = &replicas
			_, err = k.clientset.AppsV1().StatefulSets(namespace).Update(ctx, s, metav1.UpdateOptions{})
		}
	case "replicaset":
		var r *appsv1.ReplicaSet
		r, err = k.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			r.Spec.Replicas = &replicas
			_, err = k.clientset.AppsV1().ReplicaSets(namespace).Update(ctx, r, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		return "", describeKubeError(err, kind, namespace, name)
	}
	return fmt.Sprintf("scaled %s %s/%s to %d replicas", kind, namespace, name, replicas), nil
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return strings.TrimSuffix(kind, "s")
}

func describeKubeError(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s not found", kind, namespace, name)
	}
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("%s %s/%s: RBAC forbids this — check deploy/rbac.yaml grants this namespace/verb: %w", kind, namespace, name, err)
	}
	return fmt.Errorf("%s %s/%s: %w", kind, namespace, name, err)
}

// logKubeClientInit logs whether kubectl tools are available this run, and
// which specific capabilities RBAC actually granted.
func logKubeClientInit(log *zap.Logger, kc *kubeClient, err error) {
	if kc == nil {
		log.Info("kubectl tools unavailable, skipping", zap.Error(err))
		return
	}
	log.Info("kubectl tools enabled",
		zap.Strings("readable_kinds", kc.perms.readableKindsList()),
		zap.Strings("restartable_kinds", kc.perms.restartableKindsList()),
		zap.Strings("scalable_kinds", kc.perms.scalableKindsList()),
		zap.Bool("read_pod_logs", kc.perms.canReadPodLogs),
		zap.Bool("read_secrets", kc.perms.canReadSecrets))
}
