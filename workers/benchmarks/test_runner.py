import importlib.util
import json
import os
import pathlib
import sys
import shutil
import tempfile
import tarfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent


def load_runner():
    if str(ROOT) not in sys.path:
        sys.path.insert(0, str(ROOT))
    spec = importlib.util.spec_from_file_location("runner", ROOT / "runner.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def base_config(workspace: pathlib.Path) -> dict:
    return {
        "schema_version": 1,
        "run_id": "01900000-0000-7000-8000-000000000020",
        "dataset": "terminal-bench/terminal-bench@3.0.0",
        "harness": "codex",
        "workspace": str(workspace),
        "task_ids": ["html-js-filter"],
        "candidate_count": 2,
        "concurrency": 2,
        "seed": 5,
        "generation_model": "local-model",
        "generation_api_base": "http://127.0.0.1:8000/v1",
    }


class RunnerValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.runner = load_runner()
        self.workspace = ROOT / "testdata" / "workspace"

    def test_rejects_unknown_task_and_bad_uuid(self) -> None:
        config = base_config(self.workspace) | {"task_ids": ["nonexistent"]}
        with self.assertRaises(self.runner.RunnerError) as caught:
            self.runner.validate_run_config(config)
        self.assertIn("unknown Terminal-Bench", str(caught.exception))
        config = base_config(self.workspace) | {"run_id": "not-a-uuid"}
        with self.assertRaises(self.runner.RunnerError):
            self.runner.validate_run_config(config)

    def test_requires_api_base_shape(self) -> None:
        config = base_config(self.workspace) | {"generation_api_base": "http://host:8000/v1?x=1"}
        with self.assertRaises(self.runner.RunnerError):
            self.runner.validate_run_config(config)

    def test_verifier_fields_rejected_without_verification(self) -> None:
        config = base_config(self.workspace) | {"verification_model": "verifier"}
        with self.assertRaises(self.runner.RunnerError) as caught:
            self.runner.validate_run_config(config)
        self.assertEqual(str(caught.exception).split(":")[0], "benchmarks.invalid_config")

    def test_oracle_rejects_generation_fields(self) -> None:
        config = base_config(self.workspace) | {"harness": "oracle", "generation_model": "x"}
        with self.assertRaises(self.runner.RunnerError):
            self.runner.validate_run_config(config)

    def test_harbor_argument_array(self) -> None:
        config = self.runner.validate_run_config(base_config(self.workspace))
        command = self.runner.harbor_command(config)
        self.assertEqual(command[0], "harbor")
        self.assertIn("--job-name", command)
        self.assertNotIn("shell", command)
        self.assertIn("--agent-kwarg", command)

    def test_harbor_command_pins_dataset_and_task_list(self) -> None:
        config = self.runner.validate_run_config(base_config(self.workspace))
        command = self.runner.harbor_command(config)
        self.assertEqual(command[0:3], ["harbor", "run", "--dataset"])
        self.assertEqual(command[3], "terminal-bench/terminal-bench@3.0.0")
        self.assertIn("--jobs-dir", command)
        self.assertIn(str(self.runner.pathlib.Path(config["workspace"]) / "harbor"), command)
        self.assertIn("--include-task-name", command)
        self.assertIn("terminal-bench/html-js-filter", command)
        self.assertNotIn("html-js-filter", command)
        self.assertNotIn("--shell", command)
        self.assertTrue(all(isinstance(item, str) for item in command))
        self.assertIn("--n-attempts", command)
        self.assertEqual(command[command.index("--n-attempts") + 1], "2")

    def test_cleanup_rejects_relative_and_foreign_ownership(self) -> None:
        with self.assertRaises(self.runner.RunnerError) as caught:
            self.runner.validate_ownership_path("ownership.json")
        self.assertEqual(str(caught.exception).split(":")[0], "benchmarks.invalid_cleanup")
        workspace = self.runner.contained_path(str(self.workspace))
        foreign = workspace / "foreign-ownership.json"
        foreign.write_text(json.dumps(self.runner.json.loads((self.workspace / "ownership.json").read_text(encoding='utf-8'))), encoding="utf-8")
        self.addCleanup(foreign.unlink)
        manifest = self.workspace / "ownership.json"
        original = manifest.read_text(encoding="utf-8")
        corrupted = self.runner.json.loads(original)
        corrupted["project_prefix"] = "lmw-other"
        manifest.write_text(json.dumps(corrupted), encoding="utf-8")
        self.addCleanup(manifest.write_text, original, encoding="utf-8")
        with self.assertRaises(self.runner.RunnerError):
            self.runner.validate_ownership_path(str(manifest))

    def test_task_names_qualified_for_harbor(self) -> None:
        self.assertEqual(self.runner.harbor_task_name("html-js-filter"), "terminal-bench/html-js-filter")
        self.assertEqual(self.runner.unqualified_task_id("terminal-bench/html-js-filter"), "html-js-filter")
        self.assertEqual(self.runner.unqualified_task_id("html-js-filter"), "html-js-filter")

    def test_load_trials_accepts_harbor_022_oracle_shape(self) -> None:
        workspace = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, workspace, True)
        # Harbor 0.22 writes qualified task names and no trajectory.json for
        # oracle trials (the oracle adapter only emits agent/oracle.txt).
        trial_dir = workspace / "harbor" / "lmw-01900000-0000-7000-8000-000000000020" / "html-js-filter__oracle1"
        (trial_dir / "agent").mkdir(parents=True)
        (trial_dir / "agent" / "oracle.txt").write_text("", encoding="utf-8")
        (trial_dir / "result.json").write_text(json.dumps({
            "task_name": "terminal-bench/html-js-filter",
            "trial_name": "html-js-filter__oracle1",
            "exception_info": None,
            "started_at": "2026-09-04T16:22:10Z",
            "finished_at": "2026-09-04T16:24:48Z",
            "config": {"task": {"name": "terminal-bench/html-js-filter"}},
            "verifier_result": {"rewards": {"reward": 1.0}},
        }), encoding="utf-8")
        config = self.runner.validate_run_config({
            "schema_version": 1,
            "run_id": "01900000-0000-7000-8000-000000000020",
            "dataset": "terminal-bench/terminal-bench@3.0.0",
            "harness": "oracle",
            "workspace": str(workspace),
            "task_ids": ["html-js-filter"],
            "candidate_count": 1,
            "concurrency": 1,
            "seed": 0,
        })
        trials, by_task = self.runner.load_trials(config)
        self.assertEqual(len(trials), 1)
        self.assertTrue(trials[0]["official_pass"])
        self.assertIsNone(trials[0]["instruction"])
        self.assertIsNone(trials[0]["trajectory"])
        self.assertEqual(trials[0]["trajectory_path"], "")
        self.assertEqual(by_task["html-js-filter"][0]["candidate_index"], 0)

    def test_load_trials_normalizes_official_rewards(self) -> None:
        config = self.runner.validate_run_config(base_config(self.workspace))
        trials, by_task = self.runner.load_trials(config)
        self.assertEqual(len(trials), 2)
        first = next(trial for trial in trials if trial["candidate_index"] == 0)
        self.assertTrue(first["official_pass"])
        self.assertEqual(first["metrics"]["official_rewards"], {"reward": 1.0})
        self.assertEqual(first["metrics"]["tokens"]["input_tokens"], 10)
        self.assertEqual(first["metrics"]["wall_seconds"], 2.0)
        self.assertIn("Filter js from html.", first["instruction"])
        self.assertTrue((self.workspace / first["trajectory_path"]).is_file())
        self.assertEqual(len(by_task["html-js-filter"]), 2)

    def test_incomplete_candidate_pool_fails(self) -> None:
        config = self.runner.validate_run_config(base_config(self.workspace))
        removed = self.workspace / "harbor/lmw-01900000-0000-7000-8000-000000000020/trial-b/result.json"
        contents = removed.read_text(encoding="utf-8")
        os.remove(removed)
        self.addCleanup(removed.write_text, contents, encoding="utf-8")
        with self.assertRaises(self.runner.RunnerError) as caught:
            self.runner.load_trials(config)
        self.assertIn("incomplete candidate pool", str(caught.exception))

    def test_bootstrap_metrics_are_deterministic(self) -> None:
        config = self.runner.validate_run_config(base_config(self.workspace))
        _, by_task = self.runner.load_trials(config)
        metrics = self.runner.bootstrap_metrics(by_task, config["seed"])
        self.assertEqual(metrics["method"], "task-stratified-bootstrap-percentile")
        self.assertEqual(metrics["samples"], 10_000)
        self.assertLessEqual(metrics["estimates"]["pass_at_1"]["lower"], metrics["estimates"]["pass_at_1"]["point"])
        self.assertGreaterEqual(metrics["estimates"]["pass_at_1"]["upper"], metrics["estimates"]["pass_at_1"]["point"])
        self.assertIsNone(metrics["estimates"]["verifier_pass_rate"])

    def test_write_bundle_contains_top_level_files(self) -> None:
        workspace = pathlib.Path(self.workspace)
        bundle = self.runner.write_bundle(workspace)
        self.assertTrue(bundle.name == "result-bundle.tar.gz")
        with tarfile.open(bundle, "r:gz") as archive:
            names = archive.getnames()
            self.assertIn("summary.json", names)
            self.assertIn("trials.json", names)
            self.assertIn("ownership.json", names)


if __name__ == "__main__":
    unittest.main()
