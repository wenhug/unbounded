// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJournalTailArgs(t *testing.T) {
	t.Parallel()

	require.Empty(t, JournalTailArgs(LogOptions{}))
	require.Equal(t, []string{"--follow"}, JournalTailArgs(LogOptions{Follow: true}))
	require.Equal(t, []string{"--lines=20"}, JournalTailArgs(LogOptions{Lines: 20}))
	require.Equal(t, []string{"--follow", "--lines=5"}, JournalTailArgs(LogOptions{Follow: true, Lines: 5}))
	require.Empty(t, JournalTailArgs(LogOptions{Lines: -1}), "a non-positive count means no limit")
}

func TestStartLogStreamReadsOutputAndReapsOnClose(t *testing.T) {
	t.Parallel()

	r, err := StartLogStream(t.Context(), "sh", "-c", "echo line")
	require.NoError(t, err)

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "line\n", string(data))
	require.NoError(t, r.Close())
}

func TestStartLogStreamReportsAMissingCommand(t *testing.T) {
	t.Parallel()

	_, err := StartLogStream(t.Context(), "/nonexistent/unbounded-test-command")
	require.Error(t, err)
}
