// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// UnmountBelow lazily unmounts every mount at or below the given prefixes,
// deepest first. Pod volumes (tmpfs secrets, projected volumes) are mounted
// under /var/lib/kubelet, and in container mode they propagate there from the
// container and outlive it; they would make removing the directory fail or,
// worse, delete through a bind mount.
//
// mountInfo is the mount table (empty means /proc/self/mountinfo) and run
// executes host commands (nil uses the real host).
func UnmountBelow(ctx context.Context, log *slog.Logger, run none.Runner, mountInfo string, prefixes ...string) error {
	log = Logger(log)

	if mountInfo == "" {
		mountInfo = "/proc/self/mountinfo"
	}

	f, err := os.Open(mountInfo)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	defer f.Close() //nolint:errcheck // read only

	var mounts []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}

		mp := unescapeMount(fields[4])
		for _, p := range prefixes {
			if mp == p || strings.HasPrefix(mp, p+"/") {
				mounts = append(mounts, mp)
			}
		}
	}

	if err := sc.Err(); err != nil {
		return err
	}

	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })

	if run == nil {
		run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
			return executil.OutputCmd(ctx, log, name, args...)
		}
	}

	var errs []error

	for _, mp := range mounts {
		if _, err := run(ctx, log, "umount", "-l", mp); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", mp, err))
		}
	}

	return errors.Join(errs...)
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces and tabs.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
