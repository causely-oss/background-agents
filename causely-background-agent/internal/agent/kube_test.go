package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func fakeKubeClient(t *testing.T, objs ...any) *kubeClient {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for _, o := range objs {
		switch v := o.(type) {
		case *appsv1.Deployment:
			if _, err := cs.AppsV1().Deployments(v.Namespace).Create(context.Background(), v, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}
		case *corev1.ConfigMap:
			if _, err := cs.CoreV1().ConfigMaps(v.Namespace).Create(context.Background(), v, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed configmap: %v", err)
			}
		case *corev1.Secret:
			if _, err := cs.CoreV1().Secrets(v.Namespace).Create(context.Background(), v, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed secret: %v", err)
			}
		default:
			t.Fatalf("fakeKubeClient: unsupported seed object %T", o)
		}
	}
	return &kubeClient{clientset: cs}
}

// TestCheckAccessAnyNamespace_FallsBackToScopeNamespaces guards the fix for
// namespace-scoped RBAC never being detected: deploy/rbac.yaml grants
// mutate/secrets via namespaced RoleBindings on purpose, so checking only
// cluster-scoped access (empty namespace) must not be the only path checked.
func TestCheckAccessAnyNamespace_FallsBackToScopeNamespaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		// Only actually allowed when scoped to the "causely" namespace —
		// cluster-wide (empty namespace) and any other namespace are denied,
		// matching a namespaced RoleBinding that grants access in one
		// specific namespace only.
		review.Status.Allowed = review.Spec.ResourceAttributes.Namespace == "causely"
		return true, review, nil
	})

	if checkAccessAnyNamespace(cs, "apps", "deployments", "patch", nil) {
		t.Error("checkAccessAnyNamespace() with no scope namespaces should be false when only a namespaced grant exists")
	}
	if checkAccessAnyNamespace(cs, "apps", "deployments", "patch", []string{"other-namespace"}) {
		t.Error("checkAccessAnyNamespace() should be false when the grant is scoped to a namespace not in scopeNamespaces")
	}
	if !checkAccessAnyNamespace(cs, "apps", "deployments", "patch", []string{"other-namespace", "causely"}) {
		t.Error("checkAccessAnyNamespace() should be true when one of scopeNamespaces matches the namespaced grant")
	}
}

// TestCheckAccessAnyNamespace_ClusterWideGrantNeedsNoScopeNamespace guards
// the read-permission case: a ClusterRoleBinding grant (empty namespace) must
// still be detected even with no scope namespaces configured.
func TestCheckAccessAnyNamespace_ClusterWideGrantNeedsNoScopeNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = review.Spec.ResourceAttributes.Namespace == ""
		return true, review, nil
	})

	if !checkAccessAnyNamespace(cs, "", "pods", "get", nil) {
		t.Error("checkAccessAnyNamespace() should be true for a cluster-wide grant even with no scope namespaces")
	}
}

func TestKubeClient_GetResource_Deployment(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "causely"}}
	k := fakeKubeClient(t, dep)

	out, err := k.GetResource(context.Background(), "deployment", "causely", "gateway")
	if err != nil {
		t.Fatalf("GetResource() error = %v", err)
	}
	if !strings.Contains(out, `"name": "gateway"`) {
		t.Errorf("GetResource() = %s, want it to contain the deployment name", out)
	}
}

func TestKubeClient_GetResource_PluralAndCaseInsensitiveKind(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "causely"}}
	k := fakeKubeClient(t, dep)

	if _, err := k.GetResource(context.Background(), "Deployments", "causely", "gateway"); err != nil {
		t.Errorf("GetResource(\"Deployments\") error = %v, want normalization to handle plural/case", err)
	}
}

func TestKubeClient_GetResource_RejectsSecretKind(t *testing.T) {
	k := fakeKubeClient(t)
	_, err := k.GetResource(context.Background(), "secret", "causely", "otel-collector-conf")
	if err == nil {
		t.Fatal("GetResource(\"secret\") expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubectl_get_secret_keys") {
		t.Errorf("error = %v, want it to point at kubectl_get_secret_keys", err)
	}
}

func TestKubeClient_GetResource_UnsupportedKind(t *testing.T) {
	k := fakeKubeClient(t)
	if _, err := k.GetResource(context.Background(), "customresourcedefinition", "causely", "x"); err == nil {
		t.Fatal("expected an error for an unsupported kind")
	}
}

func TestKubeClient_GetResource_NotFound(t *testing.T) {
	k := fakeKubeClient(t)
	_, err := k.GetResource(context.Background(), "deployment", "causely", "does-not-exist")
	if err == nil {
		t.Fatal("expected a not-found error")
	}
}

func TestKubeClient_GetSecretKeys_NeverReturnsValues(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "otel-collector-conf", Namespace: "causely"},
		Data: map[string][]byte{
			"otel-collector-config.yaml": []byte("super-secret-content"),
		},
	}
	k := fakeKubeClient(t, secret)

	out, err := k.GetSecretKeys(context.Background(), "causely", "otel-collector-conf")
	if err != nil {
		t.Fatalf("GetSecretKeys() error = %v", err)
	}
	if strings.Contains(out, "super-secret-content") {
		t.Fatal("GetSecretKeys() leaked secret value content")
	}
	var keys []string
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "otel-collector-config.yaml" {
		t.Errorf("keys = %v, want [\"otel-collector-config.yaml\"]", keys)
	}
}

func TestKubeClient_RolloutRestart_PatchesAnnotation(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "causely"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{}},
	}
	k := fakeKubeClient(t, dep)

	if _, err := k.RolloutRestart(context.Background(), "deployment", "causely", "gateway"); err != nil {
		t.Fatalf("RolloutRestart() error = %v", err)
	}

	updated, err := k.clientset.AppsV1().Deployments("causely").Get(context.Background(), "gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if _, ok := updated.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; !ok {
		t.Error("RolloutRestart() did not set the restartedAt annotation")
	}
}

func TestKubeClient_RolloutRestart_RejectsUnsupportedKind(t *testing.T) {
	k := fakeKubeClient(t)
	if _, err := k.RolloutRestart(context.Background(), "service", "causely", "gateway"); err == nil {
		t.Fatal("expected an error for a non-restartable kind")
	}
}

func TestKubeClient_ScaleResource_SetsReplicas(t *testing.T) {
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "causely"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	k := fakeKubeClient(t, dep)

	if _, err := k.ScaleResource(context.Background(), "deployment", "causely", "gateway", 10); err != nil {
		t.Fatalf("ScaleResource() error = %v", err)
	}

	updated, err := k.clientset.AppsV1().Deployments("causely").Get(context.Background(), "gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 10 {
		t.Errorf("Spec.Replicas = %v, want 10", updated.Spec.Replicas)
	}
}

func TestKubeClient_ScaleResource_RejectsNegativeReplicas(t *testing.T) {
	k := fakeKubeClient(t)
	if _, err := k.ScaleResource(context.Background(), "deployment", "causely", "gateway", -1); err == nil {
		t.Fatal("expected an error for negative replicas")
	}
}

func TestNormalizeKind(t *testing.T) {
	tests := map[string]string{
		"Deployment":  "deployment",
		"deployments": "deployment",
		" Pods ":      "pod",
		"ConfigMaps":  "configmap",
	}
	for in, want := range tests {
		if got := normalizeKind(in); got != want {
			t.Errorf("normalizeKind(%q) = %q, want %q", in, got, want)
		}
	}
}
