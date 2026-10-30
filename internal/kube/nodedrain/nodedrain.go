// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nodedrain cordons, drains and uncordons a Kubernetes Node, and waits
// for it to report Ready, using only client-go. It follows the semantics of
// kubectl drain that matter for taking a node out of service: DaemonSet pods
// and mirror pods are left alone, finished pods are ignored, pods that no
// controller would recreate are refused, and everything else is evicted through
// the Eviction API so PodDisruptionBudgets are honored.
//
// The identity that calls it needs, on top of the node authorizer's own-node
// permissions, create on pods/eviction. See the Drainer documentation.
package nodedrain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// DefaultDrainTimeout bounds a whole drain.
	DefaultDrainTimeout = 10 * time.Minute
	// DefaultPollInterval is the wait between checks.
	DefaultPollInterval = 2 * time.Second

	mirrorPodAnnotation = "kubernetes.io/config.mirror"
)

// Drainer works on one cluster through Client.
//
// RBAC. The daemon authenticates as system:node:<name> in the system:nodes
// group plus the daemon group. With the Node authorizer enabled that already
// allows reading pods and patching its own Node. Eviction is not part of it:
// the cluster must grant create on pods/eviction (apiGroup "") to the daemon
// group, for example in the ClusterRole unbounded-daemon-controller-machine
// defined in deploy/machina/07-bootstrapper-rbac.yaml.tmpl. Where the Node
// authorizer is not in use, get and list on pods and get, list and patch on
// nodes are needed as well.
type Drainer struct {
	Client kubernetes.Interface
	Log    *slog.Logger
	// Timeout bounds Drain. Zero means DefaultDrainTimeout.
	Timeout time.Duration
	// PollInterval is the wait between checks. Zero means DefaultPollInterval.
	PollInterval time.Duration
}

func (d *Drainer) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}

	return slog.Default()
}

func (d *Drainer) poll() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}

	return DefaultPollInterval
}

// IsUnschedulable reports whether the node is cordoned.
func (d *Drainer) IsUnschedulable(ctx context.Context, node string) (bool, error) {
	n, err := d.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get node %s: %w", node, err)
	}

	return n.Spec.Unschedulable, nil
}

// Cordon marks the node unschedulable. It reports whether it changed the node,
// so a caller can leave a node an operator had already cordoned cordoned.
func (d *Drainer) Cordon(ctx context.Context, node string) (bool, error) {
	return d.setUnschedulable(ctx, node, true)
}

// Uncordon marks the node schedulable again.
func (d *Drainer) Uncordon(ctx context.Context, node string) error {
	_, err := d.setUnschedulable(ctx, node, false)

	return err
}

func (d *Drainer) setUnschedulable(ctx context.Context, node string, value bool) (bool, error) {
	current, err := d.IsUnschedulable(ctx, node)
	if err != nil {
		return false, err
	}

	if current == value {
		return false, nil
	}

	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, value)

	if _, err := d.Client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("patch node %s unschedulable=%t: %w", node, value, withRBACHint(err, "patch on nodes"))
	}

	d.log().Info("node scheduling changed", "node", node, "unschedulable", value)

	return true, nil
}

// Drain evicts the pods that must leave the node and waits until they are gone.
// The node should be cordoned first. A pod that no controller manages makes
// Drain fail before anything is evicted.
func (d *Drainer) Drain(ctx context.Context, node string) error {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pods, err := d.podsToEvict(ctx, node)
	if err != nil {
		return err
	}

	if len(pods) == 0 {
		d.log().Info("no pods to evict", "node", node)

		return nil
	}

	d.log().Info("evicting pods", "node", node, "pods", len(pods))

	type progress struct {
		pod     corev1.Pod
		evicted bool
	}

	pending := make([]*progress, 0, len(pods))
	for _, p := range pods {
		pending = append(pending, &progress{pod: p})
	}

	for {
		remaining := pending[:0]

		for _, p := range pending {
			gone, err := d.step(ctx, &p.pod, &p.evicted)
			if err != nil {
				return err
			}

			if !gone {
				remaining = append(remaining, p)
			}
		}

		pending = remaining
		if len(pending) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			names := make([]string, 0, len(pending))
			for _, p := range pending {
				names = append(names, p.pod.Namespace+"/"+p.pod.Name)
			}

			return fmt.Errorf("drain node %s: %d pod(s) still present after %s: %s: %w",
				node, len(pending), timeout, strings.Join(names, ", "), ctx.Err())
		case <-time.After(d.poll()):
		}
	}
}

// step advances one pod and reports whether it is gone. A 429 is a
// PodDisruptionBudget saying not yet, and is retried until the drain times out.
func (d *Drainer) step(ctx context.Context, pod *corev1.Pod, evicted *bool) (bool, error) {
	cur, err := d.Client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("get pod %s/%s: %w", pod.Namespace, pod.Name, withRBACHint(err, "get on pods"))
	}

	if cur.UID != pod.UID {
		// Replaced under the same name; the one on this node is gone.
		return true, nil
	}

	if *evicted {
		return false, nil
	}

	err = d.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	})

	switch {
	case err == nil:
		*evicted = true
		return false, nil
	case apierrors.IsNotFound(err):
		return true, nil
	case apierrors.IsTooManyRequests(err):
		d.log().Info("eviction blocked by a disruption budget, will retry", "pod", pod.Namespace+"/"+pod.Name)
		return false, nil
	default:
		return false, fmt.Errorf("evict pod %s/%s: %w", pod.Namespace, pod.Name, withRBACHint(err, "create on pods/eviction"))
	}
}

func (d *Drainer) podsToEvict(ctx context.Context, node string) ([]corev1.Pod, error) {
	list, err := d.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", node, withRBACHint(err, "list on pods"))
	}

	var (
		evict     []corev1.Pod
		unmanaged []string
	)

	for _, p := range list.Items {
		switch {
		case p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed:
			continue
		case isMirrorPod(&p), isDaemonSetPod(&p):
			continue
		case metav1.GetControllerOf(&p) == nil:
			unmanaged = append(unmanaged, p.Namespace+"/"+p.Name)
		default:
			evict = append(evict, p)
		}
	}

	if len(unmanaged) > 0 {
		sort.Strings(unmanaged)

		return nil, fmt.Errorf("node %s runs pods that no controller would recreate (delete them or move them first): %s",
			node, strings.Join(unmanaged, ", "))
	}

	return evict, nil
}

func isMirrorPod(p *corev1.Pod) bool {
	_, ok := p.Annotations[mirrorPodAnnotation]

	return ok
}

func isDaemonSetPod(p *corev1.Pod) bool {
	ref := metav1.GetControllerOf(p)

	return ref != nil && ref.Kind == "DaemonSet"
}

// ReadyError reports why WaitReady gave up.
type ReadyError struct {
	Node   string
	Reason string
}

func (e *ReadyError) Error() string { return fmt.Sprintf("node %s is not Ready: %s", e.Node, e.Reason) }

// WaitReady polls until the node's Ready condition is True and was reported at
// or after since, or timeout passes. The heartbeat test matters: the Ready
// condition the previous kubelet last wrote stays True until the control plane
// marks the node Unknown, so Ready alone would pass for a kubelet that has not
// started.
func (d *Drainer) WaitReady(ctx context.Context, node string, since time.Time, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last string

	for {
		n, err := d.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})

		switch {
		case err != nil && ctx.Err() == nil:
			last = err.Error()
		case err == nil:
			var ok bool

			ok, last = readyAfter(n, since)
			if ok {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return &ReadyError{Node: node, Reason: fmt.Sprintf("%s (waited %s)", last, timeout)}
		case <-time.After(d.poll()):
		}
	}
}

func readyAfter(n *corev1.Node, since time.Time) (bool, string) {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}

		switch {
		case c.Status != corev1.ConditionTrue:
			return false, fmt.Sprintf("Ready=%s (%s)", c.Status, c.Reason)
		case c.LastHeartbeatTime.Time.Before(since):
			return false, "Ready=True was last reported before the new kubelet started"
		default:
			return true, ""
		}
	}

	return false, "no Ready condition reported"
}

// withRBACHint names the permission a Forbidden answer points at.
func withRBACHint(err error, permission string) error {
	if apierrors.IsForbidden(err) {
		return errors.Join(err, fmt.Errorf("the daemon identity lacks permission: %s", permission))
	}

	return err
}
