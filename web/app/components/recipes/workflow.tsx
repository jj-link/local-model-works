import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { Button } from "~/components/ui/button";
import { qk, useCancelRun, useRun } from "~/lib/queries";
import { ApiError } from "~/lib/api/client";
import type { RecipeChangeComparison } from "~/lib/api";

export const terminalRunStates: Record<string, true> = { succeeded: true, failed: true, cancelled: true, interrupted: true };
export function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
}
export function records(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value) ? value.map(record) : [];
}
export function RecipeError({ error, retry }: { error: unknown; retry?: () => void }) {
  return <div role="alert" className="border border-fault/40 bg-fault/5 p-3 text-sm text-fault">
    <p>{error instanceof ApiError ? `${error.code}: ${error.message}` : error instanceof Error ? error.message : String(error || "Request failed")}</p>
    {retry ? <Button className="mt-2" size="sm" variant="outline" onClick={retry}>Retry</Button> : null}
  </div>;
}
export function ConfigurationComparison({ changes }: { changes: RecipeChangeComparison["configuration_changes"] }) {
  return <div className="space-y-3">{changes.length ? changes.map((change) => <article key={change.path} className="border border-rule p-3">
    <h4 className="break-all font-mono text-xs">{change.path || "/"}</h4>
    <div className="mt-2 grid gap-2 md:grid-cols-2"><div><p className="lmw-label">Saved value</p><pre className="mt-1 max-h-72 overflow-auto bg-raised p-2 text-xs">{JSON.stringify(change.before, null, 2) ?? "Not present"}</pre></div><div><p className="lmw-label">Suggested or edited value</p><pre className="mt-1 max-h-72 overflow-auto bg-raised p-2 text-xs">{JSON.stringify(change.after, null, 2) ?? "Removed"}</pre></div></div>
  </article>) : <p className="text-sm text-muted">No configuration differences.</p>}</div>;
}
export function RecipeOperation({ runId, draftId, digest, repositoryId }: { runId: string; draftId?: string; digest?: string; repositoryId?: string }) {
  const run = useRun(runId);
  const cancel = useCancelRun();
  const client = useQueryClient();
  const settled = useRef("");
  useEffect(() => {
    const transition = `${runId}:${run.data?.state}`;
    if (!run.data || !terminalRunStates[run.data.state] || settled.current === transition) return;
    settled.current = transition;
    if (draftId) void client.invalidateQueries({ queryKey: qk.recipeDraft(draftId) });
    if (digest) void client.invalidateQueries({ queryKey: qk.recipe(digest) });
    if (repositoryId) void client.invalidateQueries({ queryKey: qk.recipeRepository(repositoryId) });
    void client.invalidateQueries({ queryKey: [...qk.recipeDrafts, "list"] });
    void client.invalidateQueries({ queryKey: qk.recipeRepositories, exact: true });
    void client.invalidateQueries({ queryKey: qk.recipes, exact: true });
  }, [client, runId, run.data, draftId, digest, repositoryId]);
  return <section className="control space-y-3 p-3">
    <div className="flex flex-wrap items-center justify-between gap-3"><div><p className="lmw-label">Operation</p><Link className="text-sm underline" to={`/runs/${encodeURIComponent(runId)}`}>{run.data?.kind || "Recipe operation"} · {run.data?.state || "Loading…"}</Link></div>{run.data && !terminalRunStates[run.data.state] ? <Button size="sm" variant="outline" disabled={cancel.isPending} onClick={() => cancel.mutate(runId)}>Cancel</Button> : null}</div>
    {run.isError ? <RecipeError error={run.error} retry={() => void run.refetch()} /> : null}
    {cancel.isError ? <RecipeError error={cancel.error} /> : null}
    {run.data?.error_code || run.data?.error_message ? <p role="alert" className="text-sm text-fault">{run.data.error_code}{run.data.error_code && run.data.error_message ? ": " : ""}{run.data.error_message}</p> : null}
    {run.data?.kind === "recipe-update" ? <div className="space-y-2">{records(record(run.data.progress).running_deployments).map((target, index) => <article key={`${target.source_deployment_id}:${target.rank}:${index}`} className="space-y-1 border border-rule p-2 text-xs"><p>{String(target.node_name || target.node_id)} · Rank {String(target.rank)} · {String(target.status || "pending")} · {String(target.phase || "waiting")}</p><p>Step {String(target.current_step || 0)} / {String(target.total_steps || "Unknown")}</p>{target.source_deployment_id ? <Link className="block underline" to={`/serving/deployments/${encodeURIComponent(String(target.source_deployment_id))}`}>Previously running version</Link> : null}{target.replacement_deployment_id ? <Link className="block underline" to={`/serving/deployments/${encodeURIComponent(String(target.replacement_deployment_id))}`}>Replacement running version</Link> : null}{target.error_code || target.error_message ? <p className="text-fault">{String(target.error_code || "")}: {String(target.error_message || "")}</p> : null}</article>)}</div> : null}
    {run.data?.progress ? <details><summary className="cursor-pointer text-xs">Progress details</summary><pre className="mt-2 max-h-64 overflow-auto text-xs">{JSON.stringify(run.data.progress, null, 2)}</pre></details> : null}
  </section>;
}
