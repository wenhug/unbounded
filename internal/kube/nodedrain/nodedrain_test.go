// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodedrain

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newDrainer(c *fake.Clientset) *Drainer {
	return &Drainer{
		Client:       c,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timeout:      time.Second,
		PollInterval: time.Millisecond,
	}
}

func owned(ns, name, kind string) *corev1.Pod {
	t := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID("uid-" + name),
			OwnerReferences: []metav1.OwnerReference{{Kind: kind, Name: "owner", Controller: &t}},
		},
		Spec: corev1.PodSpec{NodeName: "n1"},
	}
}

// evictionDeletes makes an eviction delete the pod, as the API server does.
func evictionDeletes(c *fake.Clientset) {
	c.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(k8stesting.CreateAction)
		if !ok || ca.GetSubresource() != "eviction" {
			return false, nil, nil
		}

		ev := ca.GetObject().(*policyv1.Eviction)

		return true, nil, c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ev.Namespace, ev.Name)
	})
}

func TestCordonAndUncordon(t *testing.T) {
	c := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	d := newDrainer(c)

	changed, err := d.Cordon(context.Background(), "n1")
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = d.Cordon(context.Background(), "n1")
	require.NoError(t, err)
	require.False(t, changed, "an already cordoned node is not changed again")

	got, err := d.IsUnschedulable(context.Background(), "n1")
	require.NoError(t, err)
	require.True(t, got)

	require.NoError(t, d.Uncordon(context.Background(), "n1"))

	got, err = d.IsUnschedulable(context.Background(), "n1")
	require.NoError(t, err)
	require.False(t, got)
}

func TestCordonMissingNode(t *testing.T) {
	_, err := newDrainer(fake.NewSimpleClientset()).Cordon(context.Background(), "n1")
	require.Error(t, err)
}

func TestDrainEvictsManagedPodsOnly(t *testing.T) {
	mirror := owned("kube-system", "mirror", "Node")
	mirror.Annotations = map[string]string{mirrorPodAnnotation: "x"}

	done := owned("default", "done", "Job")
	done.Status.Phase = corev1.PodSucceeded

	c := fake.NewSimpleClientset(
		owned("default", "web", "ReplicaSet"),
		owned("default", "db", "StatefulSet"),
		owned("kube-system", "proxy", "DaemonSet"),
		mirror, done,
	)
	evictionDeletes(c)

	require.NoError(t, newDrainer(c).Drain(context.Background(), "n1"))

	left, err := c.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	var names []string
	for _, p := range left.Items {
		names = append(names, p.Name)
	}

	require.ElementsMatch(t, []string{"proxy", "mirror", "done"}, names)
}

func TestDrainRefusesUnmanagedPodsBeforeEvicting(t *testing.T) {
	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bare"}, Spec: corev1.PodSpec{NodeName: "n1"}}
	c := fake.NewSimpleClientset(owned("default", "web", "ReplicaSet"), bare)
	evictionDeletes(c)

	err := newDrainer(c).Drain(context.Background(), "n1")
	require.ErrorContains(t, err, "default/bare")

	for _, a := range c.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			require.NotEqual(t, "eviction", ca.GetSubresource(), "nothing may be evicted when the drain is refused")
		}
	}
}

func TestDrainRetriesWhileADisruptionBudgetBlocks(t *testing.T) {
	c := fake.NewSimpleClientset(owned("default", "web", "ReplicaSet"))

	blocks := 2

	c.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		if ca.GetSubresource() != "eviction" {
			return false, nil, nil
		}

		if blocks > 0 {
			blocks--
			return true, nil, apierrors.NewTooManyRequests("pdb", 1)
		}

		return true, nil, c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "default", "web")
	})

	require.NoError(t, newDrainer(c).Drain(context.Background(), "n1"))
	require.Zero(t, blocks)
}

func TestDrainTimesOutNamingPods(t *testing.T) {
	c := fake.NewSimpleClientset(owned("default", "stuck", "ReplicaSet"))
	c.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetSubresource() != "eviction" {
			return false, nil, nil
		}

		return true, nil, apierrors.NewTooManyRequests("pdb", 1)
	})

	d := newDrainer(c)
	d.Timeout = 30 * time.Millisecond

	err := d.Drain(context.Background(), "n1")
	require.ErrorContains(t, err, "default/stuck")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDrainForbiddenNamesTheMissingPermission(t *testing.T) {
	c := fake.NewSimpleClientset(owned("default", "web", "ReplicaSet"))
	c.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetSubresource() != "eviction" {
			return false, nil, nil
		}

		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"}, "web", nil)
	})

	err := newDrainer(c).Drain(context.Background(), "n1")
	require.True(t, apierrors.IsForbidden(err))
	require.ErrorContains(t, err, "create on pods/eviction")
}

func TestDrainNoPods(t *testing.T) {
	require.NoError(t, newDrainer(fake.NewSimpleClientset()).Drain(context.Background(), "n1"))
}

func node(ready corev1.ConditionStatus, heartbeat time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: ready, LastHeartbeatTime: metav1.NewTime(heartbeat)},
		}},
	}
}

func TestWaitReady(t *testing.T) {
	start := time.Now()

	tests := []struct {
		name    string
		node    *corev1.Node
		wantErr string
	}{
		{name: "ready after start", node: node(corev1.ConditionTrue, start.Add(time.Second))},
		{name: "stale ready from the previous kubelet", node: node(corev1.ConditionTrue, start.Add(-time.Minute)), wantErr: "before the new kubelet started"},
		{name: "not ready", node: node(corev1.ConditionFalse, start.Add(time.Second)), wantErr: "Ready=False"},
		{name: "unknown", node: node(corev1.ConditionUnknown, start.Add(time.Second)), wantErr: "Ready=Unknown"},
		{name: "no condition", node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, wantErr: "no Ready condition"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newDrainer(fake.NewSimpleClientset(tt.node)).WaitReady(context.Background(), "n1", start, 30*time.Millisecond)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			var re *ReadyError
			require.ErrorAs(t, err, &re)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestWaitReadyBecomesReady(t *testing.T) {
	start := time.Now()
	c := fake.NewSimpleClientset(node(corev1.ConditionFalse, start))

	go func() {
		time.Sleep(20 * time.Millisecond)

		n := node(corev1.ConditionTrue, start.Add(time.Second))
		_, _ = c.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{}) //nolint:errcheck // test goroutine
	}()

	require.NoError(t, newDrainer(c).WaitReady(context.Background(), "n1", start, time.Second))
}
