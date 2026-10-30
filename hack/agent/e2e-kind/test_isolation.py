# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import unittest
from unittest.mock import patch

import e2e


class IsolationModeTests(unittest.TestCase):
    def test_default_is_nspawn_and_passes_no_flag(self):
        with patch.object(e2e, "ISOLATION_MODE", ""):
            self.assertEqual(e2e.isolation_mode(), "nspawn")
            args = e2e.node_config_bootstrap_args(e2e.load_node_config(""))
        self.assertNotIn("--isolation", args)

    def test_explicit_mode_adds_flag(self):
        with patch.object(e2e, "ISOLATION_MODE", "none"):
            args = e2e.node_config_bootstrap_args(e2e.load_node_config(""))
        self.assertEqual(args[args.index("--isolation") + 1], "none")

    def test_applied_config_without_isolation_is_nspawn(self):
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"MachineName": "kube1"})), "nspawn")
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"Isolation": ""})), "nspawn")
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"Isolation": "container"})), "container")

    def test_migration_message_matches_daemon(self):
        self.assertEqual(e2e.migration_message("container", "inPlace"),
                         "IsolationMigrate to container completed (inPlace)")


class SuiteTests(unittest.TestCase):
    def test_all_suite_steps_exist(self):
        e2e.validate_suites()

    def test_default_lifecycle_has_no_isolation_steps(self):
        self.assertFalse([s for s in e2e.SUITES["lifecycle"] if "isolation" in s or s.startswith("migrate")])

    def test_migration_suite_covers_required_cases(self):
        steps = e2e.SUITES["isolation-migration"]
        for step in ("migrate-to-container-inplace", "migrate-to-nspawn-inplace",
                     "migrate-to-none-reprovision", "validate-migration-rollback-to-container"):
            self.assertIn(step, steps)


if __name__ == "__main__":
    unittest.main()
