import { useEffect, useRef, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router";
import { Download } from "lucide-react";
import { Button } from "~/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { StatusDot } from "~/components/status-dot";
import * as api from "~/lib/api";
import { useRecipeRepositories, useRun } from "~/lib/queries";
import { RecipeError, RecipeOperation, terminalRunStates } from "./workflow";

export function RecipeUpdateButton({ recipeDigest, repositoryId }: { recipeDigest: string; repositoryId?: string }) {
  const repositories = useRecipeRepositories();
  const matches = (repositories.data || []).filter((repository) => repositoryId
    ? repository.id === repositoryId
    : repository.current_recipe?.digest === recipeDigest || repository.versions.some((version) => version.recipe.digest === recipeDigest));
  const repository = matches.length === 1 ? matches[0] : undefined;
  const [open, setOpen] = useState(false);
  const [reviewed, setReviewed] = useState<{ repository: api.RecipeRepository; plan: api.RecipeRepositoryUpdatePlan }>();
  const [runId, setRunId] = useState<string>();
  const run = useRun(runId);
  const serial = useRef(0);
  const controller = useRef<AbortController | undefined>(undefined);
  const prepare = useMutation({
    mutationFn: ({ id, signal }: { id: string; signal: AbortSignal }) => api.planRecipeRepositoryUpdate(id, { signal }),
    retry: false,
  });
  const install = useMutation({
    mutationFn: ({ id, ...body }: { id: string } & api.RecipeRepositoryUpdateRequest) => api.startRecipeRepositoryUpdate(id, body),
    retry: false,
  });
  useEffect(() => () => { serial.current++; controller.current?.abort(); }, []);

  const check = async () => {
    setOpen(true);
    if (runId && (!run.data || !terminalRunStates[run.data.state])) return;
    controller.current?.abort();
    const ticket = ++serial.current;
    setReviewed(undefined);
    setRunId(undefined);
    prepare.reset();
    install.reset();
    if (!repository) return;
    const request = new AbortController();
    controller.current = request;
    try {
      const plan = await prepare.mutateAsync({ id: repository.id, signal: request.signal });
      if (serial.current === ticket && !request.signal.aborted) setReviewed({ repository, plan });
    } catch { /* Keep the update error visible; never install after a failed check. */ }
  };
  const confirm = async () => {
    if (!reviewed?.plan.ready || reviewed.plan.up_to_date || install.isPending || install.isError) return;
    try {
      const accepted = await install.mutateAsync({
        id: reviewed.repository.id,
        target_digest: reviewed.plan.target_digest,
        plan_digest: reviewed.plan.plan_digest,
      });
      setRunId(accepted.run_id);
    } catch { /* An uncertain installation request must not be replayed automatically. */ }
  };
  const close = (next: boolean) => {
    if (install.isPending) return;
    setOpen(next);
    if (!next) {
      serial.current++;
      controller.current?.abort();
      prepare.reset();
    }
  };
  const plan = reviewed?.plan;
  const name = reviewed?.repository.current_recipe?.name || repository?.current_recipe?.name || "recipe";

  return <>
    <Button size="sm" variant="outline" disabled={repositories.isPending || prepare.isPending || install.isPending} onClick={() => void check()}>
      <Download aria-hidden /> Update
    </Button>
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Update {name}</DialogTitle>
          <DialogDescription>Review the devices that will receive this update.</DialogDescription>
        </DialogHeader>
        {repositories.isError ? <RecipeError error={repositories.error} retry={() => void repositories.refetch()} /> : null}
        {!repository && !repositories.isPending && !repositories.isError ? <p role="alert" className="text-sm text-fault">This recipe does not have a unique upstream repository to update from.</p> : null}
        {prepare.isPending ? <p role="status" className="py-4 text-sm text-muted">Checking for updates and existing installations…</p> : null}
        {prepare.isError ? <RecipeError error={prepare.error} retry={() => void check()} /> : null}
        {plan && !runId ? <div className="space-y-4">
          <p className="text-sm">{plan.up_to_date ? "The latest update is already installed on these devices." : <>Install version <strong>{plan.target_version}</strong> on the following devices:</>}</p>
          <ul className="divide-y divide-rule rounded-lg border border-rule px-4" aria-label="Update devices">
            {plan.installed_devices.map((device) => {
              const versions = [...new Set((reviewed?.repository.versions || []).filter((version) => device.installed_digests.includes(version.recipe.digest)).map((version) => version.recipe.version))];
              return <li key={device.node_id} className="flex items-center justify-between gap-4 py-3">
                <div><p className="font-medium">{device.node_name}</p><p className="text-xs text-muted">{device.installed_digests.includes(plan.target_digest) ? `Version ${plan.target_version} installed` : `${versions.length ? versions.join(", ") : "Existing installation"} → ${plan.target_version}`}</p></div>
                <StatusDot state={device.node_status} />
              </li>;
            })}
          </ul>
          {!plan.up_to_date ? <div className="space-y-1 text-sm text-muted">
            <p>Your settings and installation customizations are carried forward automatically.</p>
            <p>Running models stay unchanged.</p>
          </div> : null}
          {plan.diagnostics.map((diagnostic, index) => <p key={index} role={diagnostic.severity === "error" ? "alert" : undefined} className={`text-sm ${diagnostic.severity === "error" ? "text-fault" : "text-muted"}`}>{diagnostic.message}</p>)}
        </div> : null}
        {install.isError ? <><RecipeError error={install.error} /><p className="text-sm text-muted">Check <Link className="underline" to="/runs">operation history</Link> before trying again. No update is retried automatically.</p></> : null}
        {runId ? <>
          <p role="status" className="font-medium">{run.data?.state === "succeeded" ? "Update installed on all listed devices." : run.data && terminalRunStates[run.data.state] ? "The update did not finish on every device." : "Installing update…"}</p>
          <RecipeOperation runId={runId} digest={plan?.target_digest} repositoryId={reviewed?.repository.id} />
        </> : null}
        <DialogFooter>
          <Button variant="outline" disabled={install.isPending} onClick={() => close(false)}>{runId || plan?.up_to_date ? "Close" : "Cancel"}</Button>
          {plan && !plan.up_to_date && !runId ? <Button disabled={!plan.ready || install.isPending || install.isError} onClick={() => void confirm()}>{install.isPending ? "Starting update…" : "Install update"}</Button> : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </>;
}
