// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/nodelifecycle"
)

// newCmdNodeLifecycle is the generic, backend-neutral lifecycle hook command
// used by the container isolation mode's supervising unit. nspawn-lifecycle is
// separate and unchanged.
func newCmdNodeLifecycle(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "node-lifecycle",
		Short:  "Run internal node lifecycle hooks",
		Hidden: true,
	}

	phase := func(name, short string, run func(*nodelifecycle.Lifecycle, context.Context, string) error) *cobra.Command {
		return &cobra.Command{
			Use:    name + " SLOT",
			Short:  short,
			Hidden: true,
			Args:   cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := container.ValidSlot(args[0]); err != nil {
					return err
				}

				cmdCtx.Setup()

				return run(newNodeLifecycle(cmdCtx.Logger), cmd.Context(), args[0])
			},
		}
	}

	cmd.AddCommand(
		phase("pre-start", "Refresh host-side node configuration before start", (*nodelifecycle.Lifecycle).PreStart),
		phase("post-start", "Wait for the node stack after start", (*nodelifecycle.Lifecycle).PostStart),
		phase("reconcile", "Restart the node stack and run its lifecycle reconciliation", (*nodelifecycle.Lifecycle).Reconcile),
	)

	return cmd
}

// containerController adapts the container backend to nodelifecycle.Controller.
type containerController struct{ b *container.Backend }

func (c containerController) Regenerate(ctx context.Context, slot string) error {
	return c.b.Regenerate(ctx, slot)
}

func (c containerController) Running(ctx context.Context, slot string) (bool, string, error) {
	st, err := c.b.Status(ctx, isolation.DirRoot{SlotName: slot})
	return st.Running, st.Detail, err
}

func (c containerController) Restart(ctx context.Context, slot string) error {
	return c.b.Restart(ctx, isolation.DirRoot{SlotName: slot})
}

func newNodeLifecycle(log *slog.Logger) *nodelifecycle.Lifecycle {
	return &nodelifecycle.Lifecycle{
		Log:        log,
		Controller: containerController{b: &container.Backend{Log: log}},
		ReloadSystemd: func(ctx context.Context) error {
			return executil.RunCmd(ctx, log, executil.Systemctl(), "daemon-reload")
		},
	}
}
