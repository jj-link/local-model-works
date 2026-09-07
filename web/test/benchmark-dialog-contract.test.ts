import { describe, expect, it } from "vitest";

import {
  BENCHMARK_LANGUAGES,
  firstValidationError,
  requestBody,
  type CatalogState,
} from "~/components/dialogs/benchmark-dialog";

function terminalBenchState(overrides: Partial<CatalogState> = {}): CatalogState {
  return {
    benchmark: "terminal-bench",
    version: "3.0.0",
    harness: "codegen-generative",
    generationDeploymentId: "dep-1",
    generationModel: "alpha-model",
    taskIds: "html-js-filter, sparse-latent-worlds",
    candidateCount: 3,
    concurrency: 2,
    seed: 7,
    verificationDeploymentId: "",
    verificationModel: "",
    verifierRepetitions: null,
    verifierPivots: null,
    languages: [],
    promptsPerLanguage: null,
    maxTokens: null,
    temperature: null,
    reason: "",
    ...overrides,
  };
}

const catalogEntry = {
  benchmark_id: "terminal-bench",
  version: "3.0.0",
  title: "Terminal-Bench",
  harnesses: [
    { id: "oracle", title: "Oracle", accepts_openai_compatible: false, requires_generation_deployment: false },
    { id: "codegen-generative", title: "Generative agent", accepts_openai_compatible: true, requires_generation_deployment: true },
  ],
  supports_task_filters: true,
  supports_verifier: true,
  supports_repeated_candidates: true,
  task_count: 2,
  task_ids: ["html-js-filter", "sparse-latent-worlds"],
  max_concurrency: 8,
};

describe("benchmark dialog request contract", () => {
  it("builds a generative terminal-bench body with parsed task ids", () => {
    const body = requestBody(terminalBenchState());
    expect(body).toMatchObject({
      benchmark_id: "terminal-bench",
      version: "3.0.0",
      harness: "codegen-generative",
      generation_deployment_id: "dep-1",
      generation_model: "alpha-model",
      task_ids: ["html-js-filter", "sparse-latent-worlds"],
      candidate_count: 3,
      concurrency: 2,
      seed: 7,
    });
    expect(body.verification_deployment_id).toBeUndefined();
  });

  it("omits generation fields for the oracle harness", () => {
    const body = requestBody(
      terminalBenchState({ harness: "oracle", generationDeploymentId: "dep-1" }),
    );
    expect(body.generation_deployment_id).toBeUndefined();
    expect(body.generation_model).toBeUndefined();
    expect(body.candidate_count).toBeUndefined();
    expect(body.concurrency).toBeUndefined();
    expect(body.seed).toBeUndefined();
    expect(body.task_ids).toEqual(["html-js-filter", "sparse-latent-worlds"]);
  });

  it("includes verifier fields only with an engaged verification deployment", () => {
    const base = { verificationDeploymentId: "dep-2", verificationModel: "verifier-1", verifierRepetitions: 4, verifierPivots: 2 };
    expect(requestBody(terminalBenchState(base))).toMatchObject({
      verification_deployment_id: "dep-2",
      verification_model: "verifier-1",
      verifier_repetitions: 4,
      verifier_pivots: 2,
    });
    const body = requestBody(terminalBenchState({ ...base, verificationDeploymentId: "none" }));
    expect(body.verification_deployment_id).toBeUndefined();
  });

  it("maps codegen languages and engaged numeric fields", () => {
    const body = requestBody(
      terminalBenchState({
        benchmark: "lmw-code-generation",
        harness: "oracle",
        generationDeploymentId: "",
        taskIds: "",
        candidateCount: null,
        concurrency: null,
        seed: null,
        languages: ["python", "go"],
        promptsPerLanguage: 12,
        maxTokens: 1024,
        temperature: 0.5,
      }),
    );
    expect(body).toMatchObject({
      benchmark_id: "lmw-code-generation",
      languages: ["python", "go"],
      prompts_per_language: 12,
      max_tokens: 1024,
      temperature: 0.5,
    });
  });
});

describe("benchmark dialog validation contract", () => {
  it("accepts a valid generative terminal-bench state", () => {
    expect(firstValidationError(terminalBenchState(), catalogEntry, 8)).toBeNull();
  });

  it("rejects malformed, duplicate, and unknown task ids", () => {
    expect(firstValidationError(terminalBenchState({ taskIds: "bad task" }), catalogEntry, 8)).toBe(
      "task_ids must contain only letters, numbers, dot, underscore, and hyphen",
    );
    expect(firstValidationError(terminalBenchState({ taskIds: "html-js-filter,html-js-filter" }), catalogEntry, 8)).toBe(
      "task_ids must be unique",
    );
    expect(firstValidationError(terminalBenchState({ taskIds: "not-a-task" }), catalogEntry, 8)).toBe(
      "task_ids contains an unknown Terminal-Bench 3.0.0 task",
    );
    expect(firstValidationError(terminalBenchState({ taskIds: "not-a-task" }), { ...catalogEntry, task_ids: [] }, 8)).toBeNull();
  });

  it("enforces generative candidate and concurrency bounds", () => {
    expect(firstValidationError(terminalBenchState({ candidateCount: 11 }), catalogEntry, 8)).toBe(
      "candidate_count must be between 1 and 10",
    );
    expect(firstValidationError(terminalBenchState({ concurrency: 33 }), catalogEntry, 8)).toBe(
      "concurrency must be between 1 and 32",
    );
    expect(firstValidationError(terminalBenchState({ concurrency: 9 }), catalogEntry, 8)).toBe(
      "concurrency cannot exceed the benchmark max_concurrency setting",
    );
  });

  it("enforces verifier constraints against the candidate count", () => {
    const verifying = { verificationDeploymentId: "dep-2" };
    expect(firstValidationError(terminalBenchState({ candidateCount: 1, ...verifying }), catalogEntry, 8)).toBe(
      "verification requires at least two candidates",
    );
    expect(firstValidationError(terminalBenchState({ verifierRepetitions: 33, ...verifying }), catalogEntry, 8)).toBe(
      "verifier_repetitions must be between 1 and 32",
    );
    expect(firstValidationError(terminalBenchState({ verifierPivots: 4, ...verifying }), catalogEntry, 8)).toBe(
      "verifier_pivots must be between 1 and candidate_count",
    );
    expect(firstValidationError(terminalBenchState({ verifierPivots: 3, ...verifying }), catalogEntry, 8)).toBeNull();
  });

  it("skips generative checks for the oracle harness", () => {
    const oracle = terminalBenchState({ harness: "oracle", candidateCount: 99, concurrency: 99 });
    expect(firstValidationError(oracle, catalogEntry, 8)).toBeNull();
  });

  it("validates the codegen language set and ranges", () => {
    const codegen = terminalBenchState({
      benchmark: "lmw-code-generation",
      harness: "oracle",
      generationDeploymentId: "",
      taskIds: "",
      candidateCount: null,
      concurrency: null,
      seed: null,
    });
    expect(firstValidationError(codegen, undefined, undefined)).toBe("languages is required");
    expect(firstValidationError({ ...codegen, languages: ["python"], promptsPerLanguage: 257 }, undefined, undefined)).toBe(
      "prompts_per_language must be between 1 and 256",
    );
    expect(firstValidationError({ ...codegen, languages: ["python"], maxTokens: 8 }, undefined, undefined)).toBe(
      "max_tokens must be between 16 and 16384",
    );
    expect(firstValidationError({ ...codegen, languages: ["python"], temperature: 2.5 }, undefined, undefined)).toBe(
      "temperature must be between 0 and 2",
    );
    expect(firstValidationError({ ...codegen, languages: ["python"] }, undefined, undefined)).toBeNull();
  });

  it("keeps the grader language set closed", () => {
    expect([...BENCHMARK_LANGUAGES]).toEqual(["python", "javascript", "go", "rust", "cpp", "java"]);
  });
});
