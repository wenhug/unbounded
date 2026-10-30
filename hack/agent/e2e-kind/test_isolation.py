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


class ContainerUnitTests(unittest.TestCase):
    def test_slots_come_from_the_unit_names(self):
        out = (
            "unbounded-node-kube1.service loaded active running Unbounded node kube1\n"
            "unbounded-node-kube2.service loaded active running Unbounded node kube2\n"
        )
        self.assertEqual(e2e.container_slots_from_units(out), ["kube1", "kube2"])

    def test_other_units_and_blank_output_are_ignored(self):
        self.assertEqual(e2e.container_slots_from_units(""), [])
        self.assertEqual(e2e.container_slots_from_units("unbounded-containerd.service loaded active running x\n"), [])


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


class PlanStepTests(unittest.TestCase):
    def test_drift_expectation_follows_the_controller(self):
        self.assertEqual(e2e.isolation_drift_expectation("", "nspawn"), ("False", "AppliedModeMatches"))
        self.assertEqual(e2e.isolation_drift_expectation("nspawn", "nspawn"), ("False", "AppliedModeMatches"))
        self.assertEqual(e2e.isolation_drift_expectation("", "none"), ("True", "AppliedModeDiffers"))
        self.assertEqual(e2e.isolation_drift_expectation("none", "container"), ("True", "AppliedModeDiffers"))

    def test_preflight_report_skips_text_before_the_json(self):
        report = e2e.parse_preflight_report('warning: x\n{"status": "ok", "checks": [{"name": "none-backend"}]}')
        self.assertEqual(report["checks"][0]["name"], "none-backend")
        with self.assertRaises(SystemExit):
            e2e.parse_preflight_report("no json here")

    def test_every_mode_has_a_preflight_check(self):
        self.assertEqual(set(e2e.PREFLIGHT_MODE_CHECKS), set(e2e.ISOLATION_MODES))

    def test_digest_differences(self):
        before = {"/a": "1", "/b": "2", "/c": "3"}
        after = {"/a": "1", "/b": "9", "/d": "4"}
        self.assertEqual(e2e.digest_differences(before, after), ["changed /b", "removed /c", "added /d"])
        self.assertEqual(e2e.digest_differences(before, dict(before)), [])

    def test_uninstall_asset_has_the_placeholder_the_harness_replaces(self):
        self.assertIn(e2e.UNINSTALL_SCRIPT_PLACEHOLDER, e2e.UNINSTALL_SCRIPT_ASSET.read_text())

    def test_new_steps_and_suites_exist(self):
        for step in ("validate-isolation-status", "reconcile-machine-isolation-spec", "validate-isolation-preflight",
                     "validate-isolation-uninstall", "validate-migration-rollback-config",
                     "migrate-to-container-reprovision", "collect-isolation-evidence"):
            self.assertIn(step, e2e.COMMANDS)
        for suite in ("isolation-operations", "isolation-uninstall", "isolation-roundtrip", "isolation-matrix",
                      "isolation-drift", "isolation-rollback"):
            self.assertIn(suite, e2e.SUITES)

    def test_existing_suites_are_unchanged(self):
        self.assertEqual(e2e.SUITES["lifecycle"][0], "run-agent")
        self.assertNotIn("validate-isolation-status", e2e.SUITES["lifecycle"])
        self.assertNotIn("validate-isolation-status", e2e.SUITES["isolation-migration"])

    def test_nspawn_only_checks_stay_out_of_the_mode_neutral_suites(self):
        for suite in ("isolation-operations", "isolation-uninstall", "isolation-roundtrip", "isolation-matrix",
                      "isolation-drift", "isolation-rollback"):
            self.assertNotIn("validate-host-nspawn-distro", e2e.SUITES[suite])
            self.assertNotIn("validate-node-reboot-operation", e2e.SUITES[suite])

    def test_suites_that_migrate_start_in_the_default_mode(self):
        for suite in ("isolation-roundtrip", "isolation-matrix", "isolation-drift", "isolation-rollback"):
            self.assertEqual(e2e.SUITES[suite][0], "run-agent")


if __name__ == "__main__":
    unittest.main()
