import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { useBenchmarkCatalog, useCreateBenchmark, useDeployments } from "~/lib/queries";
import type { BenchmarkCatalogEntry as CatalogEntry } from "~/lib/api";

/** The six grader languages (closed set, benchmarks module). */
export const BENCHMARK_LANGUAGES = ["python", "javascript", "go", "rust", "cpp", "java"] as const;

const DEFAULT_PROMPTS = 8;
const DEFAULT_MAX_TOKENS = 512;
const DEFAULT_VERIFIER_REPETITIONS = 8;
const NO_DEPLOYMENT = "none";

export type CatalogState = {
  benchmark: string;
  version: string;
  harness: string;
  generationDeploymentId: string;
  generationModel: string;
  taskIds: string;
  candidateCount: number | null;
  concurrency: number | null;
  seed: number | null;
  verificationDeploymentId: string;
  verificationModel: string;
  verifierRepetitions: number | null;
  verifierPivots: number | null;
  languages: string[];
  promptsPerLanguage: number | null;
  maxTokens: number | null;
  temperature: number | null;
  reason: string;
};

const baseState = (): CatalogState => ({
  benchmark: "",
  version: "",
  harness: "",
  generationDeploymentId: "",
  generationModel: "",
  taskIds: "",
  candidateCount: null,
  concurrency: null,
  seed: null,
  verificationDeploymentId: "",
  verificationModel: "",
  verifierRepetitions: null,
  verifierPivots: null,
  languages: [],
  promptsPerLanguage: null,
  maxTokens: null,
  temperature: null,
  reason: "",
});

/** Terminal-Bench task identifiers: letters, digits, dot, underscore, hyphen. */
const TASK_ID_PATTERN = /^[A-Za-z0-9._-]+$/;

function parseTaskIds(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((t) => t.trim())
    .filter(Boolean);
}

function numberOr(value: string, fallback: number | null): number | null {
  if (value.trim() === "") return fallback;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}

/** Mirror of the module's validatedBenchmarkInput for values the user engaged. */
export function firstValidationError(
  state: CatalogState,
  entry: CatalogEntry | undefined,
  runnerMax: number | undefined,
): string | null {
  if (entry && state.taskIds && entry.benchmark_id === "terminal-bench") {
    const tasks = parseTaskIds(state.taskIds);
    const known = new Set(entry.task_ids ?? []);
    const seen = new Set<string>();
    for (const task of tasks) {
      if (!TASK_ID_PATTERN.test(task)) {
        return "task_ids must contain only letters, numbers, dot, underscore, and hyphen";
      }
      if (seen.has(task)) return "task_ids must be unique";
      if (known.size > 0 && !known.has(task)) {
        return "task_ids contains an unknown Terminal-Bench 3.0.0 task";
      }
      seen.add(task);
    }
  }
  if (state.benchmark === "terminal-bench" && state.harness !== "oracle") {
    const candidateCount = state.candidateCount ?? 1;
    const concurrency = state.concurrency ?? 1;
    if (candidateCount < 1 || candidateCount > 10) {
      return "candidate_count must be between 1 and 10";
    }
    if (concurrency < 1 || concurrency > 32) {
      return "concurrency must be between 1 and 32";
    }
    if (runnerMax !== undefined && concurrency > runnerMax) {
      return "concurrency cannot exceed the benchmark max_concurrency setting";
    }
    if (state.verificationDeploymentId) {
      if (candidateCount < 2) {
        return "verification requires at least two candidates";
      }
      const repetitions = state.verifierRepetitions ?? 8;
      if (repetitions < 1 || repetitions > 32) {
        return "verifier_repetitions must be between 1 and 32";
      }
      const pivots = state.verifierPivots ?? 2;
      if (pivots < 1 || pivots > candidateCount) {
        return "verifier_pivots must be between 1 and candidate_count";
      }
    }
  }
  if (state.benchmark === "lmw-code-generation") {
    const prompts = state.promptsPerLanguage ?? 8;
    const maxTokens = state.maxTokens ?? 512;
    const temperature = state.temperature ?? 0;
    if (state.languages.length === 0) return "languages is required";
    if (prompts < 1 || prompts > 256) {
      return "prompts_per_language must be between 1 and 256";
    }
    if (maxTokens < 16 || maxTokens > 16384) {
      return "max_tokens must be between 16 and 16384";
    }
    if (temperature < 0 || temperature > 2) {
      return "temperature must be between 0 and 2";
    }
  }
  return null;
}

export function requestBody(state: CatalogState) {
  const entryId = state.benchmark;
  const body: Record<string, unknown> = {
    benchmark_id: entryId,
    version: state.version,
    harness: state.harness,
  };
  if (state.generationDeploymentId && state.harness !== "oracle") {
    body.generation_deployment_id = state.generationDeploymentId;
    if (state.generationModel) body.generation_model = state.generationModel;
  }
  if (entryId === "terminal-bench") {
    const tasks = parseTaskIds(state.taskIds);
    if (tasks.length > 0) body.task_ids = tasks;
    if (state.harness !== "oracle") {
      if (state.candidateCount !== null) body.candidate_count = state.candidateCount;
      if (state.concurrency !== null) body.concurrency = state.concurrency;
      if (state.seed !== null) body.seed = state.seed;
      if (state.verificationDeploymentId && state.verificationDeploymentId !== NO_DEPLOYMENT) {
        body.verification_deployment_id = state.verificationDeploymentId;
        if (state.verificationModel) body.verification_model = state.verificationModel;
        if (state.verifierRepetitions !== null) body.verifier_repetitions = state.verifierRepetitions;
        if (state.verifierPivots !== null) body.verifier_pivots = state.verifierPivots;
      }
    }
  } else {
    body.languages = state.languages;
    if (state.promptsPerLanguage !== null) body.prompts_per_language = state.promptsPerLanguage;
    if (state.maxTokens !== null) body.max_tokens = state.maxTokens;
    if (state.temperature !== null) body.temperature = state.temperature;
  }
  if (state.reason) body.reason = state.reason;
  return body;
}

/**
 * Catalog-driven launch dialog. Benchmark + harness come from the server
 * catalog; fields the server rejects for the chosen entry are hidden and
 * reset when the selection changes (the same contract the backend enforces).
 */
export function BenchmarkDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const { data: catalog } = useBenchmarkCatalog();
  const { data: deployments } = useDeployments();
  const create = useCreateBenchmark();

  const [state, setState] = useState<CatalogState>(baseState);

  const healthy = useMemo(
    () => (deployments ?? []).filter((d) => d.observed_state === "healthy"),
    [deployments],
  );

  const benchmark = useMemo(
    () => (catalog?.benchmarks ?? []).find((b) => b.benchmark_id === state.benchmark),
    [catalog, state.benchmark],
  );

  const availability = catalog?.runner;

  useEffect(() => {
    if (open) {
      setState(baseState());
      create.reset();
    }
  }, [open, create]);

  // Reset incompatible fields whenever the benchmark entry changes (server
  // oneOf contract: oracle rejects deployments, agents reject grader fields,
  // lmw-code-generation rejects task_ids and verifier fields).
  const selectBenchmark = (id: string) => {
    const entry = (catalog?.benchmarks ?? []).find((b) => b.benchmark_id === id);
    const version = entry?.version ?? "";
    setState((prev) => ({
      ...prev,
      benchmark: id,
      version,
      harness: "",
      generationDeploymentId: "",
      taskIds: "",
      candidateCount: null,
      concurrency: null,
      seed: null,
      verificationDeploymentId: "",
      verificationModel: "",
      verifierRepetitions: null,
      verifierPivots: null,
      languages: [],
      promptsPerLanguage: null,
      maxTokens: null,
      temperature: null,
    }));
  };

  const selectHarness = (harness: string) => {
    setState((prev) => ({
      ...prev,
      harness,
      generationDeploymentId: harness === "oracle" ? "" : prev.generationDeploymentId,
      verificationDeploymentId: "",
      verificationModel: "",
      verifierRepetitions: null,
      verifierPivots: null,
      languages: [],
      promptsPerLanguage: null,
      maxTokens: null,
      temperature: null,
    }));
  };

  const isTerminal = state.benchmark === "terminal-bench";
  const isOracle = isTerminal && state.harness === "oracle";
  const isCodegen = state.benchmark === "lmw-code-generation";
  const verificationChosen =
    state.verificationDeploymentId !== "" && state.verificationDeploymentId !== NO_DEPLOYMENT;
  const validationError = useMemo(
    () => firstValidationError(state, benchmark, availability?.max_concurrency),
    [state, benchmark, availability],
  );
  const readyToSubmit = useMemo(() => {
    if (!state.benchmark || !state.harness || validationError) return false;
    if (isCodegen) return state.generationDeploymentId !== "" && state.languages.length > 0;
    if (!isTerminal) return false;
    if (!isOracle) {
      if (state.generationDeploymentId === "") return false;
      if (verificationChosen && (state.candidateCount ?? 0) < 2) return false;
    }
    return true;
  }, [state, isTerminal, isOracle, isCodegen, verificationChosen, validationError]);

  const runnerWarning = useMemo(() => {
    if (!availability || availability.configured) return null;
    return "the digest-pinned local benchmark runner is not configured";
  }, [availability]);

  const onSubmit = async () => {
    if (!readyToSubmit) return;
    try {
      const run = await create.mutateAsync(requestBody(state) as never);
      toast.success("Benchmark run launched", { description: run.kind });
      onOpenChange(false);
      navigate(`/runs/${run.id}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "benchmark launch failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Run benchmark
          </DialogTitle>
          <DialogDescription>
            Launch a pinned benchmark through the local Harbor coordinator. Availability of the
            runner and the field contract come from the server catalog.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>Benchmark</Label>
            <Select value={state.benchmark} onValueChange={selectBenchmark}>
              <SelectTrigger className="w-full" aria-label="Benchmark">
                <SelectValue placeholder={catalog ? "select benchmark" : "loading catalog…"} />
              </SelectTrigger>
              <SelectContent>
                {(catalog?.benchmarks ?? []).map((entry) => (
                  <SelectItem key={entry.benchmark_id} value={entry.benchmark_id}>
                    {entry.title} <span className="text-muted-foreground">v{entry.version}</span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {benchmark?.harnesses && benchmark.harnesses.length > 0 ? (
            <div className="grid gap-2">
              <Label>Harness</Label>
              <Select value={state.harness} onValueChange={selectHarness}>
                <SelectTrigger className="w-full" aria-label="Harness">
                  <SelectValue placeholder="select harness" />
                </SelectTrigger>
                <SelectContent>
                  {benchmark.harnesses.map((harness) => (
                    <SelectItem key={harness.id} value={harness.id}>
                      {harness.title}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : null}

          {!isOracle && !isCodegen && state.harness && state.harness !== "oracle" ? (
            <div className="grid gap-2">
              <Label>Generation deployment</Label>
              <Select
                value={state.generationDeploymentId}
                onValueChange={(v) => setState((prev) => ({ ...prev, generationDeploymentId: v }))}
              >
                <SelectTrigger className="w-full" aria-label="Generation deployment">
                  <SelectValue placeholder="select healthy deployment" />
                </SelectTrigger>
                <SelectContent>
                  {healthy.map((d) => (
                    <SelectItem key={d.id} value={d.id}>
                      <span className="flex items-baseline gap-2">
                        <span>{d.recipe_name}</span>
                        {d.endpoint ? (
                          <span className="font-mono text-[11px] text-muted-foreground">
                            {d.endpoint.host}:{d.endpoint.port}
                          </span>
                        ) : null}
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {healthy.length === 0 ? (
                <p className="font-mono text-[11px] text-muted">no healthy deployments available</p>
              ) : null}
            </div>
          ) : null}

          {!isOracle && state.harness && state.harness !== "oracle" ? (
            <div className="grid gap-2">
              <Label htmlFor="bm-gen-model">generation_model override (optional)</Label>
              <Input
                id="bm-gen-model"
                value={state.generationModel}
                onChange={(e) => setState((prev) => ({ ...prev, generationModel: e.target.value }))}
                placeholder="defaults to the deployment model"
                className="font-mono"
              />
            </div>
          ) : null}

          {isCodegen ? (
            <div className="grid gap-2">
              <Label>Languages</Label>
              <div className="flex flex-wrap gap-1.5" role="group" aria-label="Benchmark languages">
                {BENCHMARK_LANGUAGES.map((lang) => {
                  const on = state.languages.includes(lang);
                  return (
                    <button
                      key={lang}
                      type="button"
                      aria-pressed={on}
                      onClick={() =>
                        setState((prev) => ({
                          ...prev,
                          languages: on
                            ? prev.languages.filter((l) => l !== lang)
                            : [...prev.languages, lang],
                        }))
                      }
                      className={
                        on
                          ? "control rounded border border-violet/60 bg-violet/15 px-2.5 py-1 font-mono text-xs text-violet"
                          : "control rounded border border-hairline bg-raised px-2.5 py-1 font-mono text-xs text-muted hover:text-foreground"
                      }
                    >
                      {lang}
                    </button>
                  );
                })}
              </div>
              <div className="grid grid-cols-2 gap-3 pt-1">
                <div className="grid gap-2">
                  <Label htmlFor="bm-prompts">
                    prompts/language <span className="text-muted-foreground">(default {DEFAULT_PROMPTS})</span>
                  </Label>
                  <Input
                    id="bm-prompts"
                    type="number"
                    min={1}
                    max={256}
                    value={state.promptsPerLanguage ?? DEFAULT_PROMPTS}
                    onChange={(e) => setState((prev) => ({ ...prev, promptsPerLanguage: numberOr(e.target.value, 8) }))}
                    className="font-mono"
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="bm-max">
                    max_tokens <span className="text-muted-foreground">(default {DEFAULT_MAX_TOKENS})</span>
                  </Label>
                  <Input
                    id="bm-max"
                    type="number"
                    min={16}
                    max={16384}
                    value={state.maxTokens ?? DEFAULT_MAX_TOKENS}
                    onChange={(e) => setState((prev) => ({ ...prev, maxTokens: numberOr(e.target.value, 512) }))}
                    className="font-mono"
                  />
                </div>
              </div>
            </div>
          ) : null}

          {isTerminal ? (
            <>
              <div className="grid gap-2">
                <Label htmlFor="bm-tasks">
                  task_ids <span className="text-muted-foreground">(comma or newline separated; empty = full 74-task dataset)</span>
                </Label>
                <Input
                  id="bm-tasks"
                  value={state.taskIds}
                  onChange={(e) => setState((prev) => ({ ...prev, taskIds: e.target.value }))}
                  placeholder="html-js-filter, commit-message-cleanliness"
                  className="font-mono"
                />
              </div>
              {!isOracle ? (
                <>
                  <div className="grid gap-2">
                    <Label htmlFor="bm-candidates">candidate_count (1-10)</Label>
                    <Input
                      id="bm-candidates"
                      type="number"
                      min={1}
                      max={10}
                      value={state.candidateCount ?? 1}
                      onChange={(e) => setState((prev) => ({ ...prev, candidateCount: numberOr(e.target.value, 1) }))}
                      className="font-mono"
                    />
                  </div>

                  <div className="grid grid-cols-2 gap-3">
                    <div className="grid gap-2">
                      <Label htmlFor="bm-concurrency">concurrency (1-32)</Label>
                      <Input
                        id="bm-concurrency"
                        type="number"
                        min={1}
                        max={32}
                        value={state.concurrency ?? 1}
                        onChange={(e) => setState((prev) => ({ ...prev, concurrency: numberOr(e.target.value, 1) }))}
                        className="font-mono"
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="bm-seed">seed</Label>
                      <Input
                        id="bm-seed"
                        type="number"
                        value={state.seed ?? 0}
                        onChange={(e) => setState((prev) => ({ ...prev, seed: numberOr(e.target.value, 0) }))}
                        className="font-mono"
                      />
                    </div>
                  </div>

                  <div className="grid gap-2">
                    <Label>Verification deployment (optional; enforces the logprob probe)</Label>
                    <Select
                      value={state.verificationDeploymentId || NO_DEPLOYMENT}
                      onValueChange={(v) => setState((prev) => ({ ...prev, verificationDeploymentId: v === NO_DEPLOYMENT ? "" : v }))}
                    >
                      <SelectTrigger className="w-full" aria-label="Verification deployment">
                        <SelectValue placeholder="no verification" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NO_DEPLOYMENT}>no verification</SelectItem>
                        {healthy.map((d) => (
                          <SelectItem key={d.id} value={d.id}>
                            <span className="flex items-baseline gap-2">
                              <span>{d.recipe_name}</span>
                              {d.endpoint ? (
                                <span className="font-mono text-[11px] text-muted-foreground">
                                  {d.endpoint.host}:{d.endpoint.port}
                                </span>
                              ) : null}
                            </span>
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>

                  {verificationChosen ? (
                    <>
                      <div className="grid gap-2">
                        <Label htmlFor="bm-ver-model">verification_model override (optional)</Label>
                        <Input
                          id="bm-ver-model"
                          value={state.verificationModel}
                          onChange={(e) => setState((prev) => ({ ...prev, verificationModel: e.target.value }))}
                          placeholder="defaults to the deployment model"
                          className="font-mono"
                        />
                      </div>
                      <div className="grid grid-cols-2 gap-3">
                        <div className="grid gap-2">
                          <Label htmlFor="bm-repetitions">verifier_repetitions (1-32)</Label>
                          <Input
                            id="bm-repetitions"
                            type="number"
                            min={1}
                            max={32}
                            value={state.verifierRepetitions ?? DEFAULT_VERIFIER_REPETITIONS}
                            onChange={(e) => setState((prev) => ({ ...prev, verifierRepetitions: numberOr(e.target.value, 8) }))}
                            className="font-mono"
                          />
                        </div>
                        <div className="grid gap-2">
                          <Label htmlFor="bm-pivots">verifier_pivots</Label>
                          <Input
                            id="bm-pivots"
                            type="number"
                            min={1}
                            max={10}
                            value={state.verifierPivots ?? 2}
                            onChange={(e) => setState((prev) => ({ ...prev, verifierPivots: numberOr(e.target.value, 2) }))}
                            className="font-mono"
                          />
                        </div>
                      </div>
                    </>
                  ) : null}
                </>
              ) : null}
            </>
          ) : null}

          <div className="grid gap-2">
            <Label htmlFor="bm-reason">Reason (optional)</Label>
            <Input
              id="bm-reason"
              value={state.reason}
              onChange={(e) => setState((prev) => ({ ...prev, reason: e.target.value }))}
              placeholder="e.g. after abliteration regression check"
            />
          </div>

          {validationError ? (
            <p className="font-mono text-[11px] text-warn" role="alert">{validationError}</p>
          ) : runnerWarning ? (
            <p className="font-mono text-[11px] text-amber-500">{runnerWarning}</p>
          ) : availability && !availability.online ? (
            <p className="font-mono text-[11px] text-amber-500">
              the local benchmark runner is offline
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={!readyToSubmit || create.isPending}
          >
            {create.isPending ? "launching…" : "Launch benchmark"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
