package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// kubeClient wraps a Kubernetes clientset. It exists (non-nil) only when this
// pod is actually running in a cluster — see newKubeClient. Deliberately does
// NOT expose Secret values: kubectl_get_secret_keys returns key names only,
// since Secret content flowing into Claude's context risks it being echoed
// back into a PR body, Slack message, or the investigation record.
type kubeClient struct {
	clientset kubernetes.Interface
}

// newKubeClient attempts in-cluster auto-detection (the standard
// ServiceAccount token + CA cert every pod gets mounted automatically). It
// returns (nil, err) rather than a fatal error when unavailable — e.g. running
// locally outside a cluster, or a deployment that hasn't granted the RBAC in
// deploy/rbac.yaml — so the kubectl tools are simply omitted rather than
// blocking startup, matching how an unreachable MCP server degrades in
// loadMCPSources.
func newKubeClient() (*kubeClient, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("not running in-cluster: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &kubeClient{clientset: clientset}, nil
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

// logKubeClientInit logs whether kubectl tools are available this run.
func logKubeClientInit(log *zap.Logger, kc *kubeClient, err error) {
	if kc == nil {
		log.Info("kubectl tools unavailable, skipping", zap.Error(err))
		return
	}
	log.Info("kubectl tools enabled (in-cluster ServiceAccount detected)")
}
