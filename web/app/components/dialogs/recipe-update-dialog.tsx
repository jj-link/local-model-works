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
import type { RecipeUpdateDevice, RecipeUpdateRunningDeployment } from "~/lib/api";

const TERMINAL = new Set(["succeeded", "failed", "cancelled", "interrupted"]);
const INDETERMINATE_PHASES = new Set([
  "waiting_offline",
  "rolling_back",
  "restoring_old",
  "restored",
  "rollback_failed",
]);

type ProgressDevice = RecipeUpdateDevice & {
  status?: string;
  phase?: string;
  current_step?: number;
  total_steps?: number;
  error_code?: string;
  error_message?: string;
};

type ProgressRunningDeployment = RecipeUpdateRunningDeployment & {
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
  const [permissionDiffAccepted, setPermissionDiffAccepted] = useState(false);
  const runQuery = useRun(runId);
  const plannedKey = useRef("");
  const resetPlan = planMutation.reset;
  const resetStart = startMutation.reset;

  useEffect(() => {
    if (!open) {
      plannedKey.current = "";
      setRunId(undefined);
      setPermissionDiffAccepted(false);
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
  const progressDevices = useMemo(
    () => (Array.isArray(progress.installed_devices) ? (progress.installed_devices as ProgressDevice[]) : []),
    [progress.installed_devices],
  );
  const progressRunningDeployments = useMemo(
    () => (Array.isArray(progress.running_deployments)
      ? (progress.running_deployments as ProgressRunningDeployment[])
      : []),
    [progress.running_deployments],
  );
  const totalDevices = numberValue(progress.total_devices, planMutation.data?.installed_devices?.length ?? 0);
  const completedDevices = numberValue(progress.completed_devices, 0);
  const phase = String(progress.phase ?? "");
  const overallIndeterminate = INDETERMINATE_PHASES.has(phase);
  const runState = runQuery.data?.state;
  const succeeded = runState === "succeeded";
  const failed = runState !== undefined && TERMINAL.has(runState) && !succeeded;
  const planDevices: ProgressDevice[] =
    planMutation.data?.installed_devices ?? repositoryQuery.data?.installed_devices ?? [];
  const planRunningDeployments: ProgressRunningDeployment[] =
    planMutation.data?.running_deployments ?? [];
  const displayedDevices = runId ? progressDevices : planDevices;
  const displayedRunningDeployments = runId ? progressRunningDeployments : planRunningDeployments;
  const startUpdate = async () => {
    const repository = repositoryQuery.data;
    const plan = planMutation.data;
    if (!repositoryId || !repository?.observed_head_commit || !plan || !permissionDiffAccepted) return;
    try {
      const accepted = await startMutation.mutateAsync({
        id: repositoryId,
        expected_head_commit: repository.observed_head_commit,
        plan_digest: plan.plan_digest,
        permission_diff_accepted: true,
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
            {succeeded ? "Update complete" : failed ? "Update failed" : "Update recipe"}
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
            {repositoryQuery.isPending ? "Loading installed devices…" : "Fetching and validating the pinned recipe update…"}
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
                    {completedDevices} of {totalDevices} devices updated
                  </span>
                </div>
                <progress
                  className="h-2 w-full accent-primary"
                  max={Math.max(totalDevices, 1)}
                  {...(!overallIndeterminate ? { value: completedDevices } : {})}
                />
              </section>
            ) : null}

            {planMutation.data ? (
              <section className="grid gap-2" aria-labelledby="recipe-update-permissions-title">
                <div>
                  <h3 id="recipe-update-permissions-title" className="text-sm font-semibold">Permission contract</h3>
                  <p className="text-xs text-muted-foreground">
                    Approval applies only to this compiled commit. Future commits require another review.
                  </p>
                </div>
                <div className="rounded-md border border-border bg-card p-3">
                  <p className="text-xs font-medium">Candidate permissions</p>
                  <p className="mt-1 font-mono text-xs text-muted-foreground">
                    {planMutation.data.candidate_permissions.length > 0
                      ? planMutation.data.candidate_permissions.join(", ")
                      : "standard container access"}
                  </p>
                  {planMutation.data.added_permissions.length > 0 || planMutation.data.removed_permissions.length > 0 ? (
                    <div className="mt-3 grid gap-1 text-xs">
                      {planMutation.data.added_permissions.length > 0 ? (
                        <p><span className="font-medium text-destructive">Added:</span>{" "}<span className="font-mono">{planMutation.data.added_permissions.join(", ")}</span></p>
                      ) : null}
                      {planMutation.data.removed_permissions.length > 0 ? (
                        <p><span className="font-medium">Removed:</span>{" "}<span className="font-mono">{planMutation.data.removed_permissions.join(", ")}</span></p>
                      ) : null}
                    </div>
                  ) : (
                    <p className="mt-3 text-xs text-muted-foreground">No permission changes from the current recipe.</p>
                  )}
                </div>
                {!runId ? (
                  <label className="flex cursor-pointer gap-3 rounded border border-warn/35 bg-warn/5 p-3">
                    <input
                      type="checkbox"
                      checked={permissionDiffAccepted}
                      onChange={(event) => setPermissionDiffAccepted(event.target.checked)}
                      className="mt-0.5 h-4 w-4 accent-[var(--color-primary)]"
                    />
                    <span>
                      <span className="block text-sm font-semibold">Trust this exact update for local execution</span>
                      <span className="mt-1 block text-xs text-muted-foreground">
                        I reviewed the candidate permission contract and accept it for this digest.
                      </span>
                    </span>
                  </label>
                ) : null}
              </section>
            ) : null}

            <section className="grid gap-2" aria-labelledby="recipe-update-devices-title">
              <div>
                <h3 id="recipe-update-devices-title" className="text-sm font-semibold">Installed devices</h3>
                <p className="text-xs text-muted-foreground">The candidate package will be installed on every device that holds a valid version of this recipe.</p>
              </div>
              <div className="grid gap-2">
                {displayedDevices.map((device) => {
                  const devicePhase = String(device.phase ?? "");
                  const currentStep = numberValue(device.current_step, 0);
                  const totalSteps = numberValue(device.total_steps, 2);
                  const indeterminate = INDETERMINATE_PHASES.has(devicePhase);
                  return (
                    <article key={device.node_id} className="rounded-md border border-border bg-card p-3">
                      <div className="flex flex-wrap items-start justify-between gap-2">
                        <div>
                          <p className="text-sm font-medium">{device.node_name}</p>
                          <p className="text-xs text-muted-foreground">{device.node_status}</p>
                        </div>
                        {runId && devicePhase ? <span className="text-xs font-medium">{phaseLabel(devicePhase)}</span> : null}
                      </div>
                      {runId ? (
                        <progress
                          className="mt-3 h-1.5 w-full accent-primary"
                          max={Math.max(totalSteps, 1)}
                          {...(!indeterminate ? { value: currentStep } : {})}
                        />
                      ) : null}
                      {device.error_message ? (
                        <p className="mt-2 text-xs text-destructive" role="alert">
                          {device.error_code ? `${device.error_code}: ` : ""}{device.error_message}
                        </p>
                      ) : null}
                    </article>
                  );
                })}
              </div>
            </section>

            {displayedRunningDeployments.length > 0 ? (
              <section className="grid gap-2" aria-labelledby="recipe-update-deployments-title">
                <div>
                  <h3 id="recipe-update-deployments-title" className="text-sm font-semibold">Running deployments to replace</h3>
                  <p className="text-xs text-muted-foreground">Running deployments retain their current hardware placement.</p>
                </div>
                <div className="grid gap-2">
                  {displayedRunningDeployments.map((target) => (
                    <article
                      key={`${target.source_deployment_id}:${target.node_id}:${target.rank}`}
                      className="rounded-md border border-border bg-card p-3"
                    >
                      <div className="flex flex-wrap items-start justify-between gap-2">
                        <div>
                          <p className="text-sm font-medium">{target.node_name}</p>
                          <p className="font-mono text-xs text-muted-foreground">
                            rank {target.rank} · deployment {target.source_deployment_id.slice(0, 12)}
                          </p>
                        </div>
                        {runId ? <span className="text-xs font-medium">{phaseLabel(String(target.phase ?? "pending"))}</span> : null}
                      </div>
                      {target.error_message ? (
                        <p className="mt-2 text-xs text-destructive" role="alert">
                          {target.error_code ? `${target.error_code}: ` : ""}{target.error_message}
                        </p>
                      ) : null}
                    </article>
                  ))}
                </div>
              </section>
            ) : null}

            {planMutation.data && !planMutation.data.ready ? (
              <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">
                {(planMutation.data.diagnostics ?? []).map((diagnostic) => diagnostic.message).join(" · ") || "The update plan is not ready."}
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
              disabled={!planMutation.data?.ready || !permissionDiffAccepted || startMutation.isPending || planMutation.isPending}
            >
              {startMutation.isPending ? "Starting update…" : "Update recipe"}
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
