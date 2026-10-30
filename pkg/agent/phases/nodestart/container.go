// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// ConfigureContainerdTracked is ConfigureContainerd that reports whether it
// changed any file. A node stack inside an OCI container keeps its own
// containerd.service in its rootfs, like the nspawn machine, so the unit is
// written; the caller restarts containerd only when this reports a change.
func ConfigureContainerdTracked(goalState *goalstates.NodeStart) HostConfigTask {
	return &configureContainerd{goalState: goalState}
}

// ConfigureKubeletTracked is ConfigureKubelet that reports whether it changed
// any file, for the same reason as ConfigureContainerdTracked.
func ConfigureKubeletTracked(goalState *goalstates.NodeStart) HostConfigTask {
	return &configureKubelet{goalState: goalState}
}
