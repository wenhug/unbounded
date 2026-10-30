// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// DefaultMigrationReadyTimeout bounds the wait for the Kubernetes Node to
// report Ready after the target node stack starts.
const DefaultMigrationReadyTimeout = 5 * time.Minute

// nodeScheduler is what an isolation migration needs from the cluster: take the
// Node out of service and see it come back. internal/kube/nodedrain implements
// it with the daemon's own credentials.
type nodeScheduler interface {
	Cordon(ctx context.Context, node string) (changed bool, err error)
	Uncordon(ctx context.Context, node string) error
	Drain(ctx context.Context, node string) error
	WaitReady(ctx context.Context, node string, since time.Time, timeout time.Duration) error
}

// migrationEnv is everything an IsolationMigrate needs besides the host: the
// operation it belongs to and the cluster. A nil Nodes means the daemon has no
// cluster client, and a migration, which must drain, is refused before it
// changes anything.
type migrationEnv struct {
	// Operation is the MachineOperation name, recorded in the journal so a
	// restarted daemon can report the outcome on the right object.
	Operation string
	// NodeName is the Kubernetes Node to drain and watch.
	NodeName string
	Nodes    nodeScheduler
	// ReadyTimeout bounds the wait for Ready. Zero means
	// DefaultMigrationReadyTimeout.
	ReadyTimeout time.Duration
}

func (e migrationEnv) readyTimeout() time.Duration {
	if e.ReadyTimeout > 0 {
		return e.ReadyTimeout
	}

	return DefaultMigrationReadyTimeout
}

// ErrMigrationHandedOff is returned by MigrateIsolation when the migration
// continues without this daemon: a reprovision resets the host, which removes
// the daemon, and a handoff command finishes the bootstrap. The operation stays
// InProgress and the daemon that starts afterward reports it.
var ErrMigrationHandedOff = errors.New("isolation migration handed off to the reprovision unit")

const (
	// migrationJournalName is the in-place journal, next to the applied config.
	migrationJournalName = "isolation-migration.json"

	// ReprovisionDir holds what a reprovision needs to survive its own reset:
	// the journal, a copy of the agent binary and the target configuration. The
	// reset does not remove it; the daemon that reports the outcome does.
	ReprovisionDir = "/var/lib/unbounded-agent-reprovision"
	// ReprovisionJournalName, ReprovisionConfigName and ReprovisionBinaryName
	// are the files in ReprovisionDir.
	ReprovisionJournalName = "journal.json"
	ReprovisionConfigName  = "config.json"
	ReprovisionBinaryName  = "unbounded-agent"
	// ReprovisionUnit is the systemd unit that runs the handoff command.
	ReprovisionUnit = "unbounded-agent-reprovision.service"
)

// Paths are variables so tests can point them at a temporary tree.
var (
	inPlaceJournalPath    = func() string { return filepath.Join(goalstates.AgentConfigDir, migrationJournalName) }
	reprovisionDir        = func() string { return ReprovisionDir }
	reprovisionUnitDir    = func() string { return goalstates.SystemdSystemDir }
	reprovisionJournalOf  = func() string { return filepath.Join(reprovisionDir(), ReprovisionJournalName) }
	inPlaceJournalStore   = func() migrate.JournalStore { return migrate.JournalStore{Path: inPlaceJournalPath()} }
	reprovisionJournalFor = func() migrate.JournalStore { return migrate.JournalStore{Path: reprovisionJournalOf()} }
)
