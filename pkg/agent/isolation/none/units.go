// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"fmt"
	"os"
	"path/filepath"
)

// Agent-owned unit names. They differ from the distro names so the agent never
// edits or removes a unit it did not create.
const (
	UnitContainerd = "unbounded-containerd.service"
	UnitKubelet    = "unbounded-kubelet.service"
)

// SystemdDir is where the agent writes its units.
const SystemdDir = "/etc/systemd/system"

// UnitSpec holds what the unit templates need.
type UnitSpec struct {
	// BinDir is the directory the binaries are executed from, normally
	// Layout.BinDir so the units follow the current link.
	BinDir string
	// KubeletConfigPath is the kubelet configuration file.
	KubeletConfigPath string
}

// RenderContainerd returns the containerd unit.
func RenderContainerd(s UnitSpec) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=containerd container runtime (unbounded agent)
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=%s/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`, s.BinDir)
}

// RenderKubelet returns the kubelet unit. It orders after and wants the
// agent's containerd, never the distro's.
func RenderKubelet(s UnitSpec) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=Kubelet (unbounded agent)
ConditionPathExists=%[1]s/kubelet
Wants=network-online.target %[3]s
After=network-online.target %[3]s

[Service]
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
SuccessExitStatus=143
ExecStart=%[1]s/kubelet \
        --config=%[2]s \
        $KUBELET_NODE_CONFIG_ARGS \
        $KUBELET_TUNING_ARGS \
        $KUBELET_KUBECONFIG_ARGS \
        $KUBELET_EXTRA_ARGS

[Install]
WantedBy=multi-user.target
`, s.BinDir, s.KubeletConfigPath, UnitContainerd)
}

// WriteUnits writes both units into dir and returns their paths. Writes are
// atomic so a crash never leaves a half written unit.
func WriteUnits(dir string, s UnitSpec) ([]string, error) {
	units := []struct {
		name string
		body []byte
	}{
		{UnitContainerd, RenderContainerd(s)},
		{UnitKubelet, RenderKubelet(s)},
	}

	var paths []string

	for _, u := range units {
		path := filepath.Join(dir, u.name)
		if err := writeAtomic(path, u.body, 0o644); err != nil {
			return paths, err
		}

		paths = append(paths, path)
	}

	return paths, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best effort cleanup

		return err
	}

	return nil
}
