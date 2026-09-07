#!/usr/bin/env python3
"""Trusted local Terminal-Bench coordinator for Local Model Works."""

from __future__ import annotations

import dataclasses
import datetime as dt
import importlib.metadata
import json
import math
import os
import pathlib
import random
import re
import signal
import subprocess
import sys
import tarfile
import threading
import time
import uuid
from collections import defaultdict
from typing import Any
from urllib.parse import urlsplit

DATASET = "terminal-bench/terminal-bench@3.0.0"
DATASET_PACKAGE = DATASET.split("/", 1)[0]
HARNESSES = {"oracle", "codex", "mini-swe-agent"}
CODEX_ADAPTER_VERSION = "0.149.1"
TASK_ID_RE = re.compile(r"^[A-Za-z0-9._-]+$")
RESULT_PREFIX = "RESULT:"
BOOTSTRAP_SAMPLES = 10_000
MANIFEST_PATH = pathlib.Path(__file__).with_name("terminal_bench_3_tasks.json")
if not MANIFEST_PATH.is_file():
    MANIFEST_PATH = pathlib.Path(__file__).resolve().parents[2] / "modules/benchmarks/backend/terminal_bench_3_tasks.json"
TASK_IDS = tuple(json.loads(MANIFEST_PATH.read_text(encoding="utf-8")))
TASK_ID_SET = frozenset(TASK_IDS)


class RunnerError(RuntimeError):
    pass


def fail(code: str, message: str) -> RunnerError:
    return RunnerError(f"{code}: {message}")


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise fail("benchmarks.harbor_result_incomplete", f"cannot read {path}: {error}") from error


def write_json(path: pathlib.Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".new")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    temporary.replace(path)


def strict_keys(value: dict[str, Any], allowed: set[str], required: set[str]) -> None:
    unknown = sorted(set(value) - allowed)
    missing = sorted(required - set(value))
    if unknown:
        raise fail("benchmarks.invalid_config", f"unknown config fields: {', '.join(unknown)}")
    if missing:
        raise fail("benchmarks.invalid_config", f"missing config fields: {', '.join(missing)}")


def require_int(config: dict[str, Any], name: str, minimum: int, maximum: int | None = None) -> int:
    value = config.get(name)
    if isinstance(value, bool) or not isinstance(value, int):
        raise fail("benchmarks.invalid_config", f"{name} must be an integer")
    if value < minimum or (maximum is not None and value > maximum):
        upper = f" and {maximum}" if maximum is not None else ""
        raise fail("benchmarks.invalid_config", f"{name} must be between {minimum}{upper}")
    return value


def require_model(config: dict[str, Any], name: str) -> str:
    value = config.get(name)
    if not isinstance(value, str) or not value or value != value.strip():
        raise fail("benchmarks.invalid_config", f"{name} must be a trimmed non-empty string")
    return value


def require_api_base(config: dict[str, Any], name: str) -> str:
    value = config.get(name)
    if not isinstance(value, str):
        raise fail("benchmarks.invalid_config", f"{name} must be a URL")
    parsed = urlsplit(value)
    if (
        parsed.scheme != "http"
        or not parsed.hostname
        or parsed.port is None
        or parsed.path.rstrip("/") != "/v1"
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise fail("benchmarks.invalid_config", f"{name} must be an http://host:port/v1 API base")
    return value.rstrip("/")


def contained_path(raw: Any, expected: str | None = None) -> pathlib.Path:
    if not isinstance(raw, str) or not raw:
        raise fail("benchmarks.invalid_config", "workspace must be an absolute path")
    path = pathlib.Path(raw)
    if not path.is_absolute():
        raise fail("benchmarks.invalid_config", "workspace must be an absolute path")
    resolved = path.resolve(strict=True)
    if not resolved.is_dir():
        raise fail("benchmarks.invalid_config", "workspace must be a directory")
    if expected is not None and resolved != pathlib.Path(expected).resolve(strict=True):
        raise fail("benchmarks.invalid_config", "workspace does not match the assigned job workspace")
    return resolved


def validate_run_config(config: Any) -> dict[str, Any]:
    if not isinstance(config, dict):
        raise fail("benchmarks.invalid_config", "config must be an object")
    allowed = {
        "schema_version", "run_id", "dataset", "harness", "workspace", "task_ids",
        "candidate_count", "concurrency", "seed", "generation_model",
        "generation_api_base", "verification_model", "verification_api_base",
        "verifier_repetitions", "verifier_pivots",
    }
    required = {
        "schema_version", "run_id", "dataset", "harness", "workspace", "task_ids",
        "candidate_count", "concurrency", "seed",
    }
    strict_keys(config, allowed, required)
    if config.get("schema_version") != 1:
        raise fail("benchmarks.invalid_config", "schema_version must be 1")
    try:
        parsed_run_id = uuid.UUID(str(config.get("run_id")))
    except (ValueError, TypeError, AttributeError) as error:
        raise fail("benchmarks.invalid_config", "run_id must be a UUID") from error
    if str(parsed_run_id) != config.get("run_id"):
        raise fail("benchmarks.invalid_config", "run_id must use canonical lowercase UUID form")
    if config.get("dataset") != DATASET:
        raise fail("benchmarks.invalid_config", f"dataset must be {DATASET}")
    harness = config.get("harness")
    if harness not in HARNESSES:
        raise fail("benchmarks.invalid_config", "unsupported harness")
    workspace = contained_path(config.get("workspace"), os.environ.get("LMW_JOB_WORKSPACE"))
    config["workspace"] = str(workspace)

    task_ids = config.get("task_ids")
    if not isinstance(task_ids, list) or any(not isinstance(item, str) for item in task_ids):
        raise fail("benchmarks.invalid_config", "task_ids must be an array of strings")
    if len(task_ids) != len(set(task_ids)):
        raise fail("benchmarks.invalid_config", "task_ids must be unique")
    for task_id in task_ids:
        if not TASK_ID_RE.fullmatch(task_id) or task_id not in TASK_ID_SET:
            raise fail("benchmarks.invalid_config", f"unknown Terminal-Bench task {task_id!r}")

    candidate_count = require_int(config, "candidate_count", 1, 10)
    require_int(config, "concurrency", 1, 32)
    require_int(config, "seed", -2**63, 2**63 - 1)
    verifier_fields = {
        "verification_model", "verification_api_base", "verifier_repetitions", "verifier_pivots"
    }
    verification_enabled = "verification_api_base" in config
    if harness == "oracle":
        forbidden = {"generation_model", "generation_api_base"} | verifier_fields
        supplied = sorted(forbidden & set(config))
        if supplied:
            raise fail("benchmarks.invalid_config", f"oracle does not accept {', '.join(supplied)}")
    else:
        require_model(config, "generation_model")
        config["generation_api_base"] = require_api_base(config, "generation_api_base")
        if verification_enabled:
            if candidate_count < 2:
                raise fail("benchmarks.invalid_config", "verification requires at least two candidates")
            require_model(config, "verification_model")
            config["verification_api_base"] = require_api_base(config, "verification_api_base")
            require_int(config, "verifier_repetitions", 1, 32)
            require_int(config, "verifier_pivots", 1, candidate_count)
        elif verifier_fields & set(config):
            raise fail("benchmarks.invalid_config", "all verifier fields require verification_api_base")
    return config


def docker_json(command: list[str]) -> list[dict[str, Any]]:
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    output = result.stdout.strip()
    if not output:
        return []
    value = json.loads(output)
    return value if isinstance(value, list) else [value]


def docker_ids(kind: str) -> list[str]:
    noun = {"containers": "container", "networks": "network", "volumes": "volume"}[kind]
    command = ["docker", noun, "ls", "-q", "--filter", "label=com.docker.compose.project"]
    if kind == "containers":
        command.insert(3, "-a")
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def inspect_resource(kind: str, resource_id: str) -> dict[str, Any] | None:
    noun = {"containers": "container", "networks": "network", "volumes": "volume"}[kind]
    try:
        values = docker_json(["docker", noun, "inspect", resource_id])
    except subprocess.CalledProcessError:
        return None
    return values[0] if values else None


def resource_labels(kind: str, inspection: dict[str, Any]) -> dict[str, str]:
    if kind == "containers":
        labels = inspection.get("Config", {}).get("Labels", {})
    else:
        labels = inspection.get("Labels", {})
    return labels if isinstance(labels, dict) else {}


def snapshot_ownership(workspace: pathlib.Path, run_id: str) -> dict[str, Any]:
    prefix = f"lmw-{run_id}"
    ownership: dict[str, Any] = {"schema_version": 1, "run_id": run_id, "project_prefix": prefix}
    for kind in ("containers", "networks", "volumes"):
        owned: list[dict[str, str]] = []
        for resource_id in docker_ids(kind):
            inspection = inspect_resource(kind, resource_id)
            if inspection is None:
                continue
            project = resource_labels(kind, inspection).get("com.docker.compose.project", "")
            if project.startswith(prefix):
                owned.append({"id": resource_id, "project": project})
        ownership[kind] = sorted(owned, key=lambda item: (item["project"], item["id"]))
    write_json(workspace / "ownership.json", ownership)
    return ownership


def validate_ownership_path(raw_path: str) -> tuple[pathlib.Path, dict[str, Any]]:
    path = pathlib.Path(raw_path)
    if not path.is_absolute() or path.name != "ownership.json":
        raise fail("benchmarks.invalid_cleanup", "ownership path must name an absolute ownership.json")
    resolved = path.resolve(strict=True)
    workspace = contained_path(os.environ.get("LMW_JOB_WORKSPACE"))
    if resolved.parent != workspace:
        raise fail("benchmarks.invalid_cleanup", "ownership manifest is outside the job workspace")
    value = load_json(resolved)
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        raise fail("benchmarks.invalid_cleanup", "ownership manifest schema_version must be 1")
    try:
        run_id = str(uuid.UUID(str(value.get("run_id"))))
    except (ValueError, TypeError, AttributeError) as error:
        raise fail("benchmarks.invalid_cleanup", "ownership manifest run_id must be a UUID") from error
    if run_id != value.get("run_id") or value.get("project_prefix") != f"lmw-{run_id}":
        raise fail("benchmarks.invalid_cleanup", "ownership manifest prefix does not match run_id")
    return resolved, value


def cleanup_owned(raw_path: str) -> dict[str, int]:
    _, ownership = validate_ownership_path(raw_path)
    prefix = ownership["project_prefix"]
    removed = {"containers": 0, "networks": 0, "volumes": 0}
    for kind in ("containers", "networks", "volumes"):
        entries = ownership.get(kind)
        if not isinstance(entries, list):
            raise fail("benchmarks.invalid_cleanup", f"ownership {kind} must be an array")
        noun = {"containers": "container", "networks": "network", "volumes": "volume"}[kind]
        for entry in entries:
            if not isinstance(entry, dict) or set(entry) != {"id", "project"}:
                raise fail("benchmarks.invalid_cleanup", f"invalid {kind} ownership entry")
            resource_id, recorded_project = entry["id"], entry["project"]
            if not isinstance(resource_id, str) or not isinstance(recorded_project, str) or not recorded_project.startswith(prefix):
                raise fail("benchmarks.invalid_cleanup", f"invalid {kind} ownership scope")
            inspection = inspect_resource(kind, resource_id)
            if inspection is None:
                continue
            actual_project = resource_labels(kind, inspection).get("com.docker.compose.project", "")
            if actual_project != recorded_project or not actual_project.startswith(prefix):
                raise fail("benchmarks.cleanup_scope_violation", f"{kind} {resource_id} changed ownership")
            command = ["docker", noun, "rm"]
            if kind in {"containers", "volumes"}:
                command.append("--force")
            command.append(resource_id)
            subprocess.run(command, check=True, capture_output=True, text=True)
            removed[kind] += 1
    return removed


def harbor_task_name(task_id: str) -> str:
    """Harbor identifies dataset tasks as <dataset-package>/<task-name>."""
    return f"{DATASET_PACKAGE}/{task_id}"


def unqualified_task_id(value: Any) -> Any:
    if isinstance(value, str) and value.startswith(DATASET_PACKAGE + "/"):
        return value[len(DATASET_PACKAGE) + 1:]
    return value


def harbor_command(config: dict[str, Any]) -> list[str]:
    run_id = config["run_id"]
    workspace = pathlib.Path(config["workspace"])
    command = [
        "harbor", "run",
        "--dataset", DATASET,
        "--agent", config["harness"],
        "--n-attempts", str(config["candidate_count"]),
        "--n-concurrent", str(config["concurrency"]),
        "--job-name", f"lmw-{run_id}",
        "--jobs-dir", str(workspace / "harbor"),
    ]
    for task_id in config["task_ids"]:
        command.extend(["--include-task-name", harbor_task_name(task_id)])
    if config["harness"] != "oracle":
        command.extend(["--model", f"openai/{config['generation_model']}"])
    if config["harness"] == "codex":
        command.extend(["--agent-kwarg", f"version={CODEX_ADAPTER_VERSION}"])
    return command


def parse_datetime(value: Any) -> dt.datetime | None:
    if not isinstance(value, str):
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def first_user_text(value: Any) -> str | None:
    if isinstance(value, dict):
        role = value.get("role")
        if role in {"user", "human"}:
            content = value.get("content") or value.get("text") or value.get("message")
            if isinstance(content, str) and content.strip():
                return content.strip()
            if isinstance(content, list):
                texts = [item.get("text", "") for item in content if isinstance(item, dict)]
                joined = "\n".join(text for text in texts if text).strip()
                if joined:
                    return joined
        for child in value.values():
            text = first_user_text(child)
            if text:
                return text
    elif isinstance(value, list):
        for child in value:
            text = first_user_text(child)
            if text:
                return text
    return None


def task_instruction(result: dict[str, Any], trajectory: Any, harness: str) -> str | None:
    task = result.get("config", {}).get("task", {})
    if isinstance(task, dict):
        raw_path = task.get("path")
        if isinstance(raw_path, str):
            instruction_path = pathlib.Path(raw_path) / "instruction.md"
            if instruction_path.is_file():
                instruction = instruction_path.read_text(encoding="utf-8").strip()
                if instruction:
                    return instruction
    instruction = first_user_text(trajectory)
    if instruction:
        return instruction
    if harness == "oracle":
        # Oracle trials execute the packaged reference solution through the
        # oracle adapter, which produces no transcript, so no instruction
        # evidence exists on disk.
        return None
    raise fail("benchmarks.harbor_result_incomplete", "task instruction is missing")


def persist_runner_error(workspace: pathlib.Path, error: Exception) -> None:
    """Persist the failure next to the trial artifacts. Tailing stderr after
    the container exits is racy, but the workspace bind outlives it."""
    try:
        (workspace / "runner-error.txt").write_text(f"{error}\n", encoding="utf-8")
    except OSError:
        pass


def token_metrics(result: dict[str, Any]) -> dict[str, Any]:
    contexts: list[dict[str, Any]] = []
    if isinstance(result.get("agent_result"), dict):
        contexts.append(result["agent_result"])
    for step in result.get("step_results") or []:
        if isinstance(step, dict) and isinstance(step.get("agent_result"), dict):
            contexts.append(step["agent_result"])
    totals: dict[str, Any] = {"input_tokens": 0, "cache_tokens": 0, "output_tokens": 0, "cost_usd": 0.0}
    seen = {key: False for key in totals}
    source_keys = {
        "input_tokens": "n_input_tokens", "cache_tokens": "n_cache_tokens",
        "output_tokens": "n_output_tokens", "cost_usd": "cost_usd",
    }
    for context in contexts:
        for destination, source in source_keys.items():
            value = context.get(source)
            if isinstance(value, (int, float)) and not isinstance(value, bool):
                totals[destination] += value
                seen[destination] = True
    return {key: value for key, value in totals.items() if seen[key]}


def official_pass(rewards: Any) -> tuple[bool, dict[str, float]]:
    if not isinstance(rewards, dict) or not rewards:
        raise fail("benchmarks.harbor_result_incomplete", "official verifier reward is missing")
    normalized: dict[str, float] = {}
    for key, value in rewards.items():
        if not isinstance(key, str) or isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
            raise fail("benchmarks.harbor_result_incomplete", "official verifier reward is invalid")
        normalized[key] = float(value)
    score = normalized.get("reward")
    if score is None:
        if len(normalized) != 1:
            raise fail("benchmarks.harbor_result_incomplete", "official verifier reward is ambiguous")
        score = next(iter(normalized.values()))
    return score >= 1.0, normalized


def load_trials(config: dict[str, Any]) -> tuple[list[dict[str, Any]], dict[str, list[dict[str, Any]]]]:
    workspace = pathlib.Path(config["workspace"])
    job_dir = workspace / "harbor" / f"lmw-{config['run_id']}"
    result_paths = sorted(path for path in job_dir.glob("*/result.json") if path.is_file())
    if not result_paths:
        raise fail("benchmarks.harbor_result_incomplete", "Harbor produced no trial results")
    grouped_raw: dict[str, list[tuple[pathlib.Path, dict[str, Any]]]] = defaultdict(list)
    for result_path in result_paths:
        result = load_json(result_path)
        if not isinstance(result, dict) or result.get("exception_info") is not None:
            raise fail("benchmarks.harbor_result_incomplete", f"trial failed: {result_path.parent.name}")
        task_id = unqualified_task_id(result.get("task_name"))
        if not isinstance(task_id, str) or task_id not in TASK_ID_SET:
            raise fail("benchmarks.harbor_result_incomplete", f"trial has unknown task: {task_id!r}")
        grouped_raw[task_id].append((result_path, result))
    expected_tasks = config["task_ids"] or list(TASK_IDS)
    if set(grouped_raw) != set(expected_tasks):
        raise fail("benchmarks.harbor_result_incomplete", "Harbor task results do not match the requested manifest")

    trials: list[dict[str, Any]] = []
    by_task: dict[str, list[dict[str, Any]]] = {}
    for task_id in expected_tasks:
        raw_trials = sorted(grouped_raw[task_id], key=lambda item: (str(item[1].get("trial_name", "")), item[0].parent.name))
        if len(raw_trials) != config["candidate_count"]:
            raise fail("benchmarks.harbor_result_incomplete", f"task {task_id} has an incomplete candidate pool")
        normalized_task: list[dict[str, Any]] = []
        for candidate_index, (result_path, result) in enumerate(raw_trials):
            trajectory_path = result_path.parent / "agent" / "trajectory.json"
            has_trajectory = trajectory_path.is_file()
            trajectory = load_json(trajectory_path) if has_trajectory else None
            instruction = task_instruction(result, trajectory, config["harness"])
            verifier_result = result.get("verifier_result")
            rewards = verifier_result.get("rewards") if isinstance(verifier_result, dict) else None
            passed, normalized_rewards = official_pass(rewards)
            tokens = token_metrics(result)
            started = parse_datetime(result.get("started_at"))
            finished = parse_datetime(result.get("finished_at"))
            wall_seconds = (finished - started).total_seconds() if started and finished and finished >= started else None
            trial = {
                "task_id": task_id,
                "candidate_index": candidate_index,
                "official_pass": passed,
                "verifier_selected": False,
                "verifier_score": None,
                "trajectory_path": trajectory_path.relative_to(workspace).as_posix() if has_trajectory else "",
                "instruction": instruction,
                "trajectory": trajectory,
                "metrics": {
                    "official_rewards": normalized_rewards,
                    "timings": {
                        key: result.get(key)
                        for key in ("started_at", "finished_at", "environment_setup", "agent_setup", "agent_execution", "verifier")
                        if result.get(key) is not None
                    },
                    "tokens": tokens,
                    "wall_seconds": wall_seconds,
                },
            }
            trials.append(trial)
            normalized_task.append(trial)
        by_task[task_id] = normalized_task
    return trials, by_task


def verifier_preflight(api_base: str, model: str) -> dict[str, int]:
    from openai import OpenAI

    client = OpenAI(base_url=api_base, api_key="EMPTY")
    response = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with exactly: 1"}],
        max_tokens=1,
        temperature=0,
        logprobs=True,
        top_logprobs=20,
    )
    content = response.choices[0].logprobs.content if response.choices and response.choices[0].logprobs else None
    if not content or not content[0].top_logprobs:
        raise fail("benchmarks.verifier_logprobs_unavailable", "verification deployment did not return top_logprobs")
    usage = response.usage
    return {
        "prompt_tokens": int(usage.prompt_tokens or 0) if usage else 0,
        "completion_tokens": int(usage.completion_tokens or 0) if usage else 0,
        "total_tokens": int(usage.total_tokens or 0) if usage else 0,
    }


def verifier_token_usage(module: Any) -> dict[str, Any]:
    usage = module.token_usage()
    if dataclasses.is_dataclass(usage):
        return dataclasses.asdict(usage)
    if hasattr(usage, "__dict__"):
        return dict(vars(usage))
    return {"value": str(usage)}


def verify_trials(config: dict[str, Any], by_task: dict[str, list[dict[str, Any]]]) -> dict[str, Any] | None:
    if "verification_api_base" not in config:
        return None
    os.environ["OPENAI_BASE_URL"] = config["verification_api_base"]
    os.environ["OPENAI_API_KEY"] = "EMPTY"
    import llm_verifier

    preflight = verifier_preflight(config["verification_api_base"], config["verification_model"])
    cache_root = pathlib.Path(config["workspace"]) / "verifier-cache"
    cache_root.mkdir(parents=True, exist_ok=True)
    before = verifier_token_usage(llm_verifier)
    for task_id, task_trials in by_task.items():
        result = llm_verifier.select(
            problem=task_trials[0]["instruction"],
            candidates=[json.dumps(trial["trajectory"], sort_keys=True) for trial in task_trials],
            criteria="terminal_bench",
            model=config["verification_model"],
            n_evaluations=config["verifier_repetitions"],
            pivots=config["verifier_pivots"],
            seed=config["seed"],
            max_workers=config["concurrency"],
            cache=str(cache_root / f"{task_id}.json"),
            progress=False,
            on_error="raise",
        )
        for index, trial in enumerate(task_trials):
            trial["verifier_score"] = float(result.scores[index])
            trial["verifier_selected"] = index == result.index
            trial["metrics"]["verifier_rank"] = result.ranking.index(index) + 1
            trial["metrics"]["verifier_comparisons"] = result.n_comparisons
    return {"preflight": preflight, "token_usage_before": before, "token_usage_after": verifier_token_usage(llm_verifier)}


def percentile(sorted_values: list[float], probability: float) -> float:
    position = (len(sorted_values) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return sorted_values[lower]
    weight = position - lower
    return sorted_values[lower] * (1 - weight) + sorted_values[upper] * weight


def metric_points(by_task: dict[str, list[dict[str, Any]]]) -> dict[str, float | None]:
    task_trials = list(by_task.values())
    attempts = sum(len(trials) for trials in task_trials)
    successes = sum(int(trial["official_pass"]) for trials in task_trials for trial in trials)
    oracle = sum(any(trial["official_pass"] for trial in trials) for trials in task_trials) / len(task_trials)
    selected = [next((trial for trial in trials if trial["verifier_selected"]), None) for trials in task_trials]
    verifier = None
    if all(trial is not None for trial in selected):
        verifier = sum(int(trial["official_pass"]) for trial in selected if trial is not None) / len(selected)
    baseline = successes / attempts
    return {
        "pass_at_1": baseline,
        "verifier_pass_rate": verifier,
        "oracle_pass_rate": oracle,
        "verifier_minus_baseline": verifier - baseline if verifier is not None else None,
    }


def bootstrap_metrics(by_task: dict[str, list[dict[str, Any]]], seed: int) -> dict[str, Any]:
    task_ids = list(by_task)
    points = metric_points(by_task)
    samples: dict[str, list[float]] = {name: [] for name, point in points.items() if point is not None}
    rng = random.Random(seed)
    for _ in range(BOOTSTRAP_SAMPLES):
        sampled = {f"{index}:{task_id}": by_task[task_id] for index, task_id in enumerate(rng.choices(task_ids, k=len(task_ids)))}
        for name, value in metric_points(sampled).items():
            if value is not None:
                samples[name].append(value)
    metrics: dict[str, Any] = {}
    for name, point in points.items():
        if point is None:
            metrics[name] = None
            continue
        values = sorted(samples[name])
        metrics[name] = {"point": point, "lower": percentile(values, 0.025), "upper": percentile(values, 0.975)}
    return {
        "method": "task-stratified-bootstrap-percentile",
        "samples": BOOTSTRAP_SAMPLES,
        "seed": seed,
        "confidence": 0.95,
        "estimates": metrics,
    }


def summarize(config: dict[str, Any], trials: list[dict[str, Any]], by_task: dict[str, list[dict[str, Any]]], verifier_usage: dict[str, Any] | None) -> dict[str, Any]:
    points = metric_points(by_task)
    prompt_tokens = completion_tokens = 0
    starts: list[dt.datetime] = []
    finishes: list[dt.datetime] = []
    for trial in trials:
        tokens = trial["metrics"].get("tokens", {})
        prompt_tokens += int(tokens.get("input_tokens", 0))
        completion_tokens += int(tokens.get("output_tokens", 0))
        timings = trial["metrics"].get("timings", {})
        started, finished = parse_datetime(timings.get("started_at")), parse_datetime(timings.get("finished_at"))
        if started:
            starts.append(started)
        if finished:
            finishes.append(finished)
    wall_seconds = (max(finishes) - min(starts)).total_seconds() if starts and finishes else 0.0
    passed_count = sum(any(trial["official_pass"] for trial in task_trials) for task_trials in by_task.values())
    return {
        "schema_version": 1,
        "benchmark_id": "terminal-bench",
        "benchmark_version": "3.0.0",
        "dataset": DATASET,
        "source_checksum": "2b0442c3c583b710ca8da14c8e601b99f2f1f244",
        "harness": config["harness"],
        "task_count": len(by_task),
        "candidate_count": config["candidate_count"],
        "passed_count": passed_count,
        "pass_at_1": points["pass_at_1"],
        "verifier_pass_rate": points["verifier_pass_rate"],
        "oracle_pass_rate": points["oracle_pass_rate"],
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "total_tokens": prompt_tokens + completion_tokens,
        "wall_seconds": wall_seconds,
        "metrics": {"bootstrap": bootstrap_metrics(by_task, config["seed"]), "verifier": verifier_usage},
    }


def write_bundle(workspace: pathlib.Path) -> pathlib.Path:
    bundle = workspace / "result-bundle.tar.gz"
    entries = [
        workspace / "summary.json", workspace / "trials.json", workspace / "config.json",
        workspace / "versions.json", workspace / "dataset.json", workspace / "ownership.json",
        workspace / "harbor", workspace / "verifier-cache",
    ]
    with tarfile.open(bundle, "w:gz", format=tarfile.PAX_FORMAT) as archive:
        for entry in entries:
            if entry.exists():
                archive.add(entry, arcname=entry.relative_to(workspace).as_posix(), recursive=True)
    return bundle


def run_benchmark(raw_config: str) -> dict[str, str]:
    try:
        parsed = json.loads(raw_config)
    except json.JSONDecodeError as error:
        raise fail("benchmarks.invalid_config", f"config is not JSON: {error}") from error
    config = validate_run_config(parsed)
    workspace = pathlib.Path(config["workspace"])
    write_json(workspace / "config.json", config)
    write_json(workspace / "dataset.json", {"locator": DATASET, "task_ids": list(TASK_IDS)})
    write_json(workspace / "versions.json", {
        "harbor": importlib.metadata.version("harbor"),
        "llm-verifier": importlib.metadata.version("llm-verifier"),
        "codex_adapter": CODEX_ADAPTER_VERSION,
    })
    snapshot_ownership(workspace, config["run_id"])

    environment = os.environ.copy()
    if config["harness"] != "oracle":
        environment["OPENAI_BASE_URL"] = config["generation_api_base"]
        environment["OPENAI_API_KEY"] = "EMPTY"
    process = subprocess.Popen(harbor_command(config), env=environment)
    stopped = threading.Event()

    def track() -> None:
        while not stopped.wait(2):
            try:
                snapshot_ownership(workspace, config["run_id"])
            except Exception as error:  # ownership capture is retried until Harbor exits
                print(f"ownership snapshot failed: {error}", file=sys.stderr, flush=True)

    tracker = threading.Thread(target=track, daemon=True)
    tracker.start()
    try:
        return_code = process.wait()
    finally:
        stopped.set()
        tracker.join(timeout=5)
        snapshot_ownership(workspace, config["run_id"])
    if return_code != 0:
        raise fail("benchmarks.harbor_failed", f"Harbor exited with code {return_code}")

    try:
        trials, by_task = load_trials(config)
        verifier_usage = verify_trials(config, by_task)
        summary = summarize(config, trials, by_task, verifier_usage)
        public_trials = []
        for trial in trials:
            public_trial = dict(trial)
            public_trial.pop("instruction")
            public_trial.pop("trajectory")
            public_trials.append(public_trial)
        write_json(workspace / "trials.json", public_trials)
        write_json(workspace / "summary.json", summary)
        bundle = write_bundle(workspace)
    except Exception as error:
        persist_runner_error(workspace, error)
        raise
    return {
        "summary": "summary.json",
        "trials": "trials.json",
        "bundle": bundle.relative_to(workspace).as_posix(),
        "ownership": "ownership.json",
    }


def main(argv: list[str]) -> int:
    if len(argv) != 3 or argv[1] not in {"run", "cleanup"}:
        print("usage: runner.py run <config-json> | cleanup <ownership-json>", file=sys.stderr)
        return 2
    try:
        if argv[1] == "run":
            result = run_benchmark(argv[2])
        else:
            result = cleanup_owned(argv[2])
        print(RESULT_PREFIX + json.dumps(result, sort_keys=True), flush=True)
        return 0
    except RunnerError as error:
        print(str(error), file=sys.stderr, flush=True)
        return 1
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        print(f"benchmarks.runner_failed: {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
