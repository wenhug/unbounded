# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import unittest
import unittest.mock
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

    def test_preflight_report_check_accepts_the_modes_own_check(self):
        report = {"status": "ok", "checks": [{"name": "none-backend", "severity": "ok"}]}
        e2e.check_isolation_preflight_report("none", report)

    def test_preflight_report_check_rejects_a_missing_or_foreign_check(self):
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report("none", {"checks": [{"name": "nspawn-runtime"}]})
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report(
                "none", {"checks": [{"name": "none-backend"}, {"name": "container-backend"}]})
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report(
                "none", {"checks": [{"name": "none-backend", "severity": "error", "message": "x"}]})

    def test_bootstrap_report_is_only_asserted_for_the_bootstrap_mode(self):
        import pathlib
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            saved = pathlib.Path(tmp) / "unbounded-agent-preflight.json"
            saved.write_text(json.dumps({"checks": [{"name": "nspawn-runtime", "severity": "ok"}]}))
            with patch.object(e2e, "VM_DIR", pathlib.Path(tmp)):
                with patch.dict("os.environ", {"ISOLATION_MODE": ""}):
                    # An nspawn bootstrap says nothing about a node now in none mode.
                    with patch.object(e2e, "warn") as warned:
                        skipped = e2e.assert_bootstrap_preflight_mode_check("none")
                    self.assertIn("bootstrap ran in nspawn", skipped)
                    self.assertIn("SKIPPED", warned.call_args.args[0])
                    # The bootstrap mode itself is asserted, so nothing is skipped.
                    self.assertEqual(e2e.assert_bootstrap_preflight_mode_check("nspawn"), "")
                with patch.dict("os.environ", {"ISOLATION_MODE": "none"}):
                    with self.assertRaises(SystemExit):
                        e2e.assert_bootstrap_preflight_mode_check("none")

    def test_a_missing_bootstrap_report_is_reported_as_skipped(self):
        import pathlib
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            with patch.object(e2e, "VM_DIR", pathlib.Path(tmp)), patch.object(e2e, "warn") as warned:
                reason = e2e.assert_bootstrap_preflight_mode_check("none")
        self.assertEqual(reason, "no bootstrap preflight report was saved")
        self.assertIn("SKIPPED", warned.call_args.args[0])

    def test_daemon_unit_check_has_no_machine_override_outside_nspawn(self):
        seen = []

        def fake_ssh(command):
            seen.append(command)
            return ("ExecStart=/opt/b/unbounded-agent-current daemon\n"
                    "ExecStart=/opt/b/unbounded-agent-daemon-recovery.sh\n")

        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "ssh_capture", fake_ssh):
            e2e._daemon_unit_runs("/opt/b")
        self.assertNotIn("override.conf", seen[0])

        with patch.object(e2e, "ISOLATION_MODE", ""), patch.object(e2e, "ssh_capture", fake_ssh):
            with self.assertRaises(SystemExit):
                e2e._daemon_unit_runs("/opt/b")
        self.assertIn("override.conf", seen[1])

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


class NodeStackDiagnosticsTests(unittest.TestCase):
    def _run(self, mode, ssh):
        with patch.object(e2e, "ISOLATION_MODE", mode), patch.object(e2e, "ssh_capture_quiet", side_effect=ssh), \
                patch.object(e2e.subprocess, "run"):
            e2e.dump_node_stack_diagnostics()

    def test_container_mode_asks_for_the_in_container_journals(self):
        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="out", stderr="", returncode=0)

        self._run("container", ssh)
        joined = "\n".join(commands)
        self.assertIn("/opt/unbounded/runtime/runc list", joined)
        self.assertIn("journalctl --no-pager -n 150 -u kubelet -u containerd", joined)

    def test_other_modes_do_not_query_runc(self):
        for mode in ("nspawn", "none"):
            commands = []

            def ssh(command):
                commands.append(command)
                return unittest.mock.Mock(stdout="", stderr="", returncode=1)

            self._run(mode, ssh)
            self.assertTrue(commands)
            self.assertNotIn("runc", "\n".join(commands), mode)

    def test_a_failing_query_never_raises(self):
        def ssh(_command):
            raise OSError("ssh is gone")

        self._run("container", ssh)


class UninstallSuiteTests(unittest.TestCase):
    def test_the_suite_stops_at_the_script_because_reset_is_the_way_back(self):
        """The uninstall script is not a reset; `unbounded-agent reset` is the
        only way back to a clean host, and the operations suites cover reset and
        reinstall. This suite only checks what the script removes."""
        steps = e2e.SUITES["isolation-uninstall"]
        self.assertEqual(steps[-1], "validate-isolation-uninstall")
        self.assertNotIn("reinstall-agent", steps)
        self.assertNotIn("reset-host", e2e.COMMANDS)

    def test_the_uninstall_check_only_asks_for_what_the_script_owns(self):
        for mode, leftover in (("container", ["/var/lib/unbounded/containers/kube1"]),
                               ("none", [e2e.NONE_NODE_BASE])):
            with self.subTest(mode=mode), patch.object(e2e, "ISOLATION_MODE", mode), \
                    patch.object(e2e, "present_on_host", return_value=leftover):
                with self.assertRaises(SystemExit):
                    e2e.validate_uninstall_removed_node_stack("kube1")

    def test_layout_directories_are_not_the_scripts_business(self):
        with patch.object(e2e, "ISOLATION_MODE", "container"), \
                patch.object(e2e, "present_on_host", return_value=[]) as present:
            e2e.validate_uninstall_removed_node_stack("kube1")
        asked = present.call_args.args[0]
        self.assertNotIn(e2e.CONTAINER_ROOT_BASE, asked)

    def test_an_nspawn_machine_that_is_still_closing_is_waited_for(self):
        """The script returns after its own bounded wait; a node container with
        running pods can still be State=closing. e2e showed the check failing
        on that, so the machine is given time to go."""
        answers = [unittest.mock.Mock(returncode=0), unittest.mock.Mock(returncode=0),
                   unittest.mock.Mock(returncode=1)]
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e.time, "sleep") as slept, \
                patch.object(e2e, "ssh_capture_quiet", side_effect=answers):
            e2e.validate_uninstall_removed_node_stack("kube1")
        self.assertEqual(slept.call_count, 2)

    def test_an_nspawn_machine_that_never_goes_fails(self):
        clock = iter(range(0, 10000, 100))
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(returncode=0, stdout="", stderr="")):
            with self.assertRaises(SystemExit):
                e2e.validate_uninstall_removed_node_stack("kube1", "script output")


class ActiveSlotTests(unittest.TestCase):
    def test_a_container_host_names_its_slot_from_the_unit(self):
        units = unittest.mock.Mock(returncode=0, stdout="unbounded-node-kube2.service loaded active running x\n")
        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "ssh_capture_quiet", return_value=units):
            self.assertEqual(e2e.active_node_slot(), "kube2")
            self.assertEqual(e2e.alternate_nspawn_slot(), "kube1")

    def test_none_always_uses_kube1(self):
        with patch.object(e2e, "ISOLATION_MODE", "none"):
            self.assertEqual(e2e.active_node_slot(), "kube1")
            self.assertEqual(e2e.alternate_nspawn_slot(), "kube2")


class SoftRebootTests(unittest.TestCase):
    def _run(self, mode, markers):
        """Run the soft reboot check with the kubelet start marker answering from
        markers, one value per read."""
        answers = iter(markers)
        quiet = unittest.mock.Mock(side_effect=lambda command: unittest.mock.Mock(
            returncode=0, stdout=next(answers), stderr=""))
        clock = iter(range(0, 100000, 40))
        with patch.object(e2e, "ISOLATION_MODE", mode), patch.object(e2e, "ssh_capture_quiet", quiet), \
                patch.object(e2e, "ssh_capture", return_value="boot-1\n"), patch.object(e2e, "ssh_cmd"), \
                patch.object(e2e, "run_quiet"), patch.object(e2e, "create_machine_operation"), \
                patch.object(e2e, "wait_for_machine_operation_complete"), patch.object(e2e, "wait_for_node_ready"), \
                patch.object(e2e, "applied_isolation", return_value=mode), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_isolation_soft_reboot()

    def test_a_restarted_kubelet_passes_in_every_mode(self):
        for mode in ("nspawn", "none", "container"):
            with self.subTest(mode=mode):
                self._run(mode, ["100\n", "250\n"])

    def test_a_node_reboot_that_did_nothing_fails_even_though_the_node_is_ready(self):
        with self.assertRaises(SystemExit):
            self._run("container", ["100\n"] * 8)

    def test_an_unreadable_marker_before_the_reboot_fails(self):
        with self.assertRaises(SystemExit):
            self._run("nspawn", [""])

    def test_the_marker_reads_the_kubelet_unit_of_the_mode(self):
        for mode, unit in (("none", "unbounded-kubelet.service"), ("nspawn", "kubelet.service"),
                           ("container", "kubelet.service")):
            seen = []

            def ssh(command, seen=seen):
                seen.append(command)
                return unittest.mock.Mock(returncode=0, stdout="7\n", stderr="")

            with self.subTest(mode=mode), patch.object(e2e, "ISOLATION_MODE", mode), \
                    patch.object(e2e, "ssh_capture_quiet", ssh):
                self.assertEqual(e2e.node_stack_start_marker(), "7")
            self.assertIn(f"exec -- systemctl show -p ActiveEnterTimestampMonotonic --value {unit}", seen[0])


class RollbackCheckTests(unittest.TestCase):
    def test_a_none_source_is_refused_because_the_rollback_would_never_run(self):
        with patch.object(e2e, "applied_isolation", return_value="none"), \
                patch.object(e2e, "shared_config_digest") as digest:
            with self.assertRaises(SystemExit):
                e2e.validate_migration_rollback_config(None)
        digest.assert_not_called()

    def test_a_rollback_message_must_name_the_rollback_and_the_injected_fault(self):
        good = "persist applied config: x (rolled back to container)"
        e2e.assert_rollback_ran(good, "container")
        for bad in ("", "preflight nspawn: no (the container node was not changed)",
                    "start nspawn: boom (rolled back to container)"):
            with self.subTest(message=bad), self.assertRaises(SystemExit):
                e2e.assert_rollback_ran(bad, "container")

    def test_a_custom_fault_only_needs_the_rollback(self):
        e2e.assert_rollback_ran("boom (rolled back to nspawn)", "nspawn", custom_fault=True)
        with self.assertRaises(SystemExit):
            e2e.assert_rollback_ran("boom (the nspawn node was not changed)", "nspawn", custom_fault=True)


class DigestTests(unittest.TestCase):
    def test_a_kubelet_client_certificate_rotation_is_not_a_change(self):
        import pathlib
        import subprocess
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            pki = pathlib.Path(tmp) / "pki"
            pki.mkdir()
            (pki / "ca.crt").write_text("ca")
            (pki / "kubelet-client-2026-01-01-00-00-00.pem").write_text("old")
            (pki / "kubelet-client-current.pem").symlink_to("kubelet-client-2026-01-01-00-00-00.pem")
            (pathlib.Path(tmp) / "config.yaml").write_text("cfg")

            def digest():
                with patch.object(e2e, "SHARED_CONFIG_PATHS", (str(pki), str(pathlib.Path(tmp) / "config.yaml"))):
                    script = e2e.shared_config_digest_script()

                return e2e.parse_digest(subprocess.run(["sh", "-c", script], capture_output=True, text=True,
                                                       check=True).stdout)

            before = digest()
            self.assertIn(str(pki / "ca.crt"), before)
            self.assertIn(str(pathlib.Path(tmp) / "config.yaml"), before)
            self.assertFalse([p for p in before if "kubelet-client-" in p], before)

            # The kubelet rotates its certificate.
            (pki / "kubelet-client-current.pem").unlink()
            (pki / "kubelet-client-2026-02-01-00-00-00.pem").write_text("new")
            (pki / "kubelet-client-current.pem").symlink_to("kubelet-client-2026-02-01-00-00-00.pem")
            self.assertEqual(e2e.digest_differences(before, digest()), [])

            # A real configuration change is still seen.
            (pki / "ca.crt").write_text("changed")
            self.assertEqual(e2e.digest_differences(before, digest()), [f"changed {pki / 'ca.crt'}"])


class DiagnosticsRedactionTests(unittest.TestCase):
    def test_tokens_and_kubeconfig_credentials_are_removed(self):
        text = ("kubelet: token abcdef.0123456789abcdef\n"
                "users:\n- user:\n    token: s3cr3t\n    client-key-data: QUJD\n"
                '{"BootstrapToken": "zzzzzz.yyyyyyyyyyyyyyyy"}\nplain line\n')
        out = e2e.redact_diagnostics(text)
        for secret in ("abcdef.0123456789abcdef", "s3cr3t", "QUJD", "zzzzzz.yyyyyyyyyyyyyyyy"):
            self.assertNotIn(secret, out)
        self.assertIn("plain line", out)

    def test_the_dump_prints_redacted_output_and_reads_the_bundle_directory(self):
        import contextlib
        import io

        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="token abcdef.0123456789abcdef", stderr="", returncode=0)

        out = io.StringIO()
        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "ssh_capture_quiet", ssh), \
                patch.object(e2e.subprocess, "run"), contextlib.redirect_stdout(out):
            e2e.dump_node_stack_diagnostics()
        self.assertNotIn("abcdef.0123456789abcdef", out.getvalue())
        self.assertIn("<redacted-token>", out.getvalue())
        self.assertTrue(any("containers/bundles/*/config.json" in c for c in commands))


class UninstallSlotTests(unittest.TestCase):
    def test_the_uninstall_validation_uses_the_active_slot(self):
        seen = {}

        def validate(slot, output=""):
            seen["slot"] = slot

        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "active_node_slot", return_value="kube2"), \
                patch.object(e2e, "ssh_capture", return_value="Isolation mode: container\n"), \
                patch.object(e2e, "ssh_capture_quiet"), patch.object(e2e, "scp_cmd"), \
                patch.object(e2e, "validate_uninstall_removed_node_stack", validate), \
                patch.object(e2e, "run_quiet"), patch.object(e2e, "delete_machine_cr"), \
                patch.object(e2e, "VM_DIR", e2e.REPO_ROOT / "tmp"):
            (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)
            e2e.validate_isolation_uninstall()
        self.assertEqual(seen["slot"], "kube2")


class MigrationDeclaresTheSpecTests(unittest.TestCase):
    def test_the_drift_suite_creates_the_mismatch_itself(self):
        """A migration sets spec.isolation, so the drift suite edits the spec to
        an unapplied mode to make the condition appear, then sets it back."""
        steps = e2e.SUITES["isolation-drift"]
        self.assertIn("declare-isolation-mismatch", e2e.COMMANDS)

        for index, step in enumerate(steps):
            if step == "declare-isolation-mismatch":
                self.assertEqual(steps[index + 1], "reconcile-machine-isolation-spec")

    def test_the_mismatch_is_a_mode_other_than_the_applied_one(self):
        for applied, other in (("none", "nspawn"), ("container", "nspawn"), ("nspawn", "none")):
            calls = []
            view = {"spec": other, "observed": applied,
                    "drift": {"status": "True", "reason": "AppliedModeDiffers"}}
            with patch.object(e2e, "ISOLATION_MODE", applied), patch.object(e2e, "kubectl", side_effect=lambda a: calls.append(a)), \
                    patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=view):
                e2e.declare_isolation_mismatch()
            self.assertIn(f'"isolation": "{other}"', calls[0][-1], applied)

    def test_a_drift_that_does_not_appear_fails(self):
        view = {"spec": "nspawn", "observed": "none", "drift": {"status": "False", "reason": "AppliedModeMatches"}}
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "kubectl"), \
                patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=view):
            with self.assertRaises(SystemExit):
                e2e.declare_isolation_mismatch()

    def test_a_spec_the_agent_did_not_set_fails_the_migration_check(self):
        clock = iter(range(0, 10000, 100))
        view = {"spec": "", "observed": "container", "drift": {"status": "True", "reason": "AppliedModeDiffers"}}
        with patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "machine_isolation_view", return_value=view):
            with self.assertRaises(SystemExit):
                e2e.validate_spec_declares_migrated_mode("container")

    def test_a_declared_spec_passes(self):
        view = {"spec": "container", "observed": "container", "drift": {"status": "False", "reason": "AppliedModeMatches"}}
        with patch.object(e2e, "machine_isolation_view", return_value=view):
            e2e.validate_spec_declares_migrated_mode("container")


class NSpawnRuntimeTests(unittest.TestCase):
    def test_the_rollback_suite_refuses_then_installs_the_runtime_before_the_rollback_check(self):
        steps = e2e.SUITES["isolation-rollback"]
        refuse = steps.index("validate-migration-needs-nspawn-runtime")
        install = steps.index("install-nspawn-runtime")
        rollback = steps.index("validate-migration-rollback-config")
        self.assertLess(refuse, install)
        self.assertLess(install, rollback)

    def test_a_host_that_has_the_runtime_has_nothing_to_refuse(self):
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=False), \
                patch.object(e2e, "create_machine_operation") as created:
            e2e.validate_migration_needs_nspawn_runtime()
        created.assert_not_called()

    def test_refusal_for_another_reason_fails(self):
        operation = {"status": {"message": "something unrelated"}}
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=True), \
                patch.object(e2e, "isolation_node_marker", return_value="kube1"), \
                patch.object(e2e, "create_machine_operation"), \
                patch.object(e2e, "wait_for_machine_operation_failed", return_value=operation):
            with self.assertRaises(SystemExit):
                e2e.validate_migration_needs_nspawn_runtime()

    def test_the_expected_refusal_passes_when_the_node_is_untouched(self):
        operation = {"status": {"message": "host cannot run isolation mode nspawn: nspawn-runtime: ... machinectl"}}
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=True), \
                patch.object(e2e, "isolation_node_marker", return_value="kube1"), \
                patch.object(e2e, "create_machine_operation"), \
                patch.object(e2e, "wait_for_node_ready"), \
                patch.object(e2e, "wait_for_machine_operation_failed", return_value=operation):
            e2e.validate_migration_needs_nspawn_runtime()

    def test_a_refusal_that_changed_the_node_fails(self):
        operation = {"status": {"message": "host cannot run isolation mode nspawn: machinectl"}}
        markers = iter(["kube1", "kube2"])
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=True), \
                patch.object(e2e, "isolation_node_marker", side_effect=lambda: next(markers)), \
                patch.object(e2e, "create_machine_operation"), \
                patch.object(e2e, "wait_for_machine_operation_failed", return_value=operation):
            with self.assertRaises(SystemExit):
                e2e.validate_migration_needs_nspawn_runtime()


class DelayedTargetFaultTests(unittest.TestCase):
    def test_the_fault_waits_for_the_target_machine(self):
        calls = []
        answers = iter([1, 1, 0])

        def ssh(command):
            calls.append(command)
            if "machinectl show" in command:
                return unittest.mock.Mock(returncode=next(answers))
            return unittest.mock.Mock(returncode=0)

        fault = e2e.DelayedTargetFault("kube2")
        with patch.object(e2e, "ssh_capture_quiet", side_effect=ssh):
            fault._run()
        self.assertTrue(fault.injected.is_set())
        self.assertIn("kube2-applied-config.json", calls[-1])
        self.assertIn("mkdir", calls[-1])
        self.assertEqual(sum("machinectl show" in c for c in calls), 3, "polled until the machine ran")

    def test_it_gives_up_without_a_machine(self):
        clock = iter(range(0, 100000, 400))
        fault = e2e.DelayedTargetFault("kube2", timeout_secs=600)
        with patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(returncode=1)):
            fault._run()
        self.assertFalse(fault.injected.is_set())

    def test_stop_removes_the_directory_only_when_it_was_placed(self):
        calls = []
        with patch.object(e2e, "ssh_capture_quiet", side_effect=lambda c: calls.append(c)):
            idle = e2e.DelayedTargetFault("kube2")
            idle.stop()
            self.assertEqual(calls, [])

            placed = e2e.DelayedTargetFault("kube2")
            placed.injected.set()
            placed.stop()
        self.assertEqual(len(calls), 1)
        self.assertIn("rmdir", calls[0])


if __name__ == "__main__":
    unittest.main()
