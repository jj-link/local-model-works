import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
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
import {
  usePlanRecipeRepositoryUpdate,
  useRecipeRepository,
  useRun,
  useStartRecipeRepositoryUpdate,
} from "~/lib/queries";
import type { RecipeUpdateTarget } from "~/lib/api";

const TERMINAL = new Set(["succeeded", "failed", "cancelled", "interrupted"]);
const INDETERMINATE_PHASES = new Set([
  "waiting_offline",
  "rolling_back",
  "restoring_old",
  "restored",
  "rollback_failed",
]);

type ProgressTarget = RecipeUpdateTarget & {
  error_code?: string;
  error_message?: string;
};

export function RecipeUpdateDialog({
  open,
  onOpenChange,
  repositoryId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  repositoryId?: string;
}) {
  const repositoryQuery = useRecipeRepository(open ? repositoryId : undefined);
  const planMutation = usePlanRecipeRepositoryUpdate();
  const startMutation = useStartRecipeRepositoryUpdate();
  const [runId, setRunId] = useState<string>();
  const runQuery = useRun(runId);
  const plannedKey = useRef("");
  const resetPlan = planMutation.reset;
  const resetStart = startMutation.reset;

  useEffect(() => {
    if (!open) {
      plannedKey.current = "";
      setRunId(undefined);
      resetPlan();
      resetStart();
    }
  }, [open, resetPlan, resetStart]);

  useEffect(() => {
    const repository = repositoryQuery.data;
    const expectedHead = repository?.observed_head_commit;
    if (!open || !repositoryId || !expectedHead || !repository.update_supported || runId) return;
    const key = `${repositoryId}:${expectedHead}`;
    if (plannedKey.current === key) return;
    plannedKey.current = key;
    void planMutation
      .mutateAsync({ id: repositoryId, expected_head_commit: expectedHead })
      .catch((error: unknown) => {
        toast.error(error instanceof Error ? error.message : "Recipe update plan failed");
      });
  }, [open, planMutation, repositoryId, repositoryQuery.data, runId]);

  const progress = (runQuery.data?.progress ?? {}) as Record<string, unknown>;
  const hardware = useMemo(
    () => (Array.isArray(progress.hardware) ? (progress.hardware as ProgressTarget[]) : []),
    [progress.hardware],
  );
  const totalHardware = numberValue(progress.total_hardware, planMutation.data?.targets.length ?? 0);
  const completedHardware = numberValue(progress.completed_hardware, 0);
  const phase = String(progress.phase ?? "");
  const overallIndeterminate = INDETERMINATE_PHASES.has(phase);
  const runState = runQuery.data?.state;
  const succeeded = runState === "succeeded";
  const failed = runState !== undefined && TERMINAL.has(runState) && !succeeded;
  const planTargets: ProgressTarget[] = planMutation.data?.targets ?? repositoryQuery.data?.affected_hardware.flatMap((item) =>
    item.deployment_ids.map((deploymentId) => ({
      source_deployment_id: deploymentId,
      node_id: item.node_id,
      node_name: item.node_name,
      node_status: item.node_status,
      rank: 0,
      status: "pending" as const,
      phase: "fetching" as const,
      current_step: 0,
      total_steps: 5,
    })),
  ) ?? [];
  const displayedHardware = runId ? hardware : planTargets;

  const startUpdate = async () => {
    const repository = repositoryQuery.data;
    const plan = planMutation.data;
    if (!repositoryId || !repository?.observed_head_commit || !plan) return;
    try {
      const accepted = await startMutation.mutateAsync({
        id: repositoryId,
        expected_head_commit: repository.observed_head_commit,
        plan_digest: plan.plan_digest,
      });
      setRunId(accepted.run_id);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Recipe update failed to start");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold">
            {succeeded ? "Update complete" : failed ? "Update failed" : "Update recipe on current hardware"}
          </DialogTitle>
          <DialogDescription>
            {repositoryQuery.data?.current_recipe?.display_name ||
              repositoryQuery.data?.current_recipe?.name ||
              "Repository recipe"}
            {repositoryQuery.data?.observed_head_commit
              ? ` · ${repositoryQuery.data.observed_head_commit.slice(0, 12)}`
              : ""}
          </DialogDescription>
        </DialogHeader>

        {repositoryQuery.isPending || planMutation.isPending ? (
          <div className="lmw-panel-raised p-4 text-sm" role="status">
            {repositoryQuery.isPending ? "Loading affected hardware…" : "Fetching and validating the pinned recipe update…"}
          </div>
        ) : repositoryQuery.isError || planMutation.isError ? (
          <div className="rounded-md border border-destructive/40 bg-destructive/5 p-4 text-sm" role="alert">
            {errorMessage(repositoryQuery.error ?? planMutation.error, "Cannot prepare this update")}
          </div>
        ) : !repositoryQuery.data?.update_supported ? (
          <div className="rounded-md border border-border p-4 text-sm" role="status">
            Automatic updates are unavailable for this repository: {repositoryQuery.data?.update_diagnostic ?? "recipe.repository_unsupported"}.
          </div>
        ) : (
          <div className="grid gap-4">
            {runId ? (
              <section className="grid gap-2" aria-label="Overall update progress">
                <div className="flex items-center justify-between text-sm">
                  <span className="font-medium">{phaseLabel(phase)}</span>
                  <span className="text-muted-foreground">
                    {completedHardware} of {totalHardware} hardware targets complete
                  </span>
                </div>
                <progress
                  className="h-2 w-full accent-primary"
                  max={Math.max(totalHardware, 1)}
                  {...(!overallIndeterminate ? { value: completedHardware } : {})}
                />
              </section>
            ) : null}

            <section className="grid gap-2" aria-labelledby="recipe-update-hardware-title">
              <div>
                <h3 id="recipe-update-hardware-title" className="text-sm font-semibold">Hardware using this recipe</h3>
                <p className="text-xs text-muted-foreground">Placement is preserved. Hardware cannot be reselected during an update.</p>
              </div>
              {displayedHardware.length === 0 ? (
                <div className="rounded-md border border-border p-3 text-sm text-muted-foreground">No running hardware uses an older version.</div>
              ) : (
                <div className="grid gap-2">
                  {displayedHardware.map((target) => {
                    const targetPhase = String(target.phase ?? "pending");
                    const currentStep = numberValue(target.current_step, 0);
                    const totalSteps = numberValue(target.total_steps, 5);
                    const indeterminate = INDETERMINATE_PHASES.has(targetPhase);
                    return (
                      <article
                        key={`${target.source_deployment_id}:${target.node_id}:${target.rank}`}
                        className="rounded-md border border-border bg-card p-3"
                      >
                        <div className="flex flex-wrap items-start justify-between gap-2">
                          <div>
                            <p className="text-sm font-medium">{target.node_name}</p>
                            <p className="font-mono text-xs text-muted-foreground">
                              {target.node_status} · rank {target.rank} · deployment {target.source_deployment_id.slice(0, 12)}
                            </p>
                          </div>
                          <span className="text-xs font-medium">{phaseLabel(targetPhase)}</span>
                        </div>
                        {runId ? (
                          <progress
                            className="mt-3 h-1.5 w-full accent-primary"
                            max={Math.max(totalSteps, 1)}
                            {...(!indeterminate ? { value: currentStep } : {})}
                          />
                        ) : null}
                        {target.error_message ? (
                          <p className="mt-2 text-xs text-destructive" role="alert">
                            {target.error_code ? `${target.error_code}: ` : ""}{target.error_message}
                          </p>
                        ) : null}
                      </article>
                    );
                  })}
                </div>
              )}
            </section>

            {planMutation.data && !planMutation.data.ready ? (
              <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">
                {planMutation.data.diagnostics.map((diagnostic) => diagnostic.message).join(" · ") || "The update plan is not ready."}
              </div>
            ) : null}
            {failed ? (
              <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">
                {runQuery.data?.error_message || "The update did not complete. Source deployments were restored when possible."}
              </div>
            ) : null}
          </div>
        )}

        <DialogFooter>
          {runId ? (
            <Button asChild variant="outline">
              <Link to={`/runs/${runId}`}>View run details</Link>
            </Button>
          ) : null}
          {!runId ? (
            <Button
              onClick={() => void startUpdate()}
              disabled={!planMutation.data?.ready || startMutation.isPending || planMutation.isPending}
            >
              {startMutation.isPending ? "Starting update…" : "Update this hardware"}
            </Button>
          ) : null}
          <Button variant={runId && !TERMINAL.has(runState ?? "") ? "outline" : "default"} onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function numberValue(value: unknown, fallback: number): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

function phaseLabel(phase: string): string {
  return phase ? phase.replaceAll("_", " ") : "pending";
}
