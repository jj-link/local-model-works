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
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { useCreateDeployment, useNodes, usePlanDeployment, useRecipe, useRecipes } from "~/lib/queries";
import { PlanPreview } from "~/components/plan-preview";
import { stateInfo, TONE_TEXT } from "~/lib/format";
import { cn } from "~/lib/utils";

/**
 * Plan a deployment: pick an installed recipe + profile, optionally pin
 * nodes per rank, preview the plan (placement, transfers, risks,
 * conflicts), then create. Full-screen review sheet below 768px.
 */
export function PlanDeploymentDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const { data: recipes } = useRecipes();
  const { data: nodes } = useNodes();
  const planMutation = usePlanDeployment();
  const createMutation = useCreateDeployment();

  const [recipeDigest, setRecipeDigest] = useState<string>("");
  const [profile, setProfile] = useState<string>("");
  const [nodeOverrides, setNodeOverrides] = useState<Record<number, string>>({});
  const { data: recipeDetail, isFetching: detailFetching } = useRecipe(recipeDigest || undefined);

  const profiles = useMemo(() => {
    const m = recipeDetail?.manifest;
    if (!m || typeof m !== "object") return [];
    const p = (m as Record<string, unknown>).profiles;
    if (!p || typeof p !== "object") return [];
    return Object.keys(p as Record<string, unknown>);
  }, [recipeDetail]);

  const nodeCount =
    ((recipeDetail?.compatibility as { node_count?: number } | undefined)?.node_count ?? 1) || 1;

  useEffect(() => {
    if (open) {
      setRecipeDigest("");
      setProfile("");
      setNodeOverrides({});
      planMutation.reset();
      createMutation.reset();
    }
  }, [open, planMutation, createMutation]);

  useEffect(() => {
    setProfile((p) => (profiles.includes(p) ? p : profiles[0] ?? ""));
  }, [profiles]);

  const plan = planMutation.data;

  const preview = async () => {
    if (!recipeDigest || !profile) return;
    const placements = Object.entries(nodeOverrides)
      .filter(([, nodeId]) => nodeId)
      .map(([rank, node_id]) => ({ rank: Number(rank), node_id: node_id as string }));
    try {
      await planMutation.mutateAsync({
        recipe_digest: recipeDigest,
        profile,
        ...(placements.length > 0 ? { placements } : {}),
      });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "plan failed");
    }
  };

  const create = async () => {
    if (!plan) return;
    try {
      const dep = await createMutation.mutateAsync({
        recipe_digest: recipeDigest,
        profile,
        plan_digest: undefined,
        ...(plan.placements.length > 0
          ? {
              placements: plan.placements.map((p) => ({ node_id: p.node_id, rank: p.rank })),
            }
          : {}),
      });
      toast.success("Deployment created", { description: `${dep.recipe_name} @ ${dep.profile}` });
      onOpenChange(false);
      navigate(`/serving/deployments/${dep.id}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "create failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Plan deployment
          </DialogTitle>
          <DialogDescription>
            Choose an installed recipe and profile. The plan shows placement, artifact
            preparation, ports, risks, and conflicts before anything is created.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label>Recipe</Label>
              <Select
                value={recipeDigest}
                onValueChange={(v) => {
                  setRecipeDigest(v);
                  setNodeOverrides({});
                  planMutation.reset();
                }}
              >
                <SelectTrigger className="w-full" aria-label="Recipe">
                  <SelectValue placeholder="select recipe" />
                </SelectTrigger>
                <SelectContent>
                  {(recipes ?? []).map((r) => (
                    <SelectItem key={r.digest} value={r.digest}>
                      <span className="flex items-baseline gap-2">
                        <span>{r.name}@{r.version}</span>
                        <span className={cn("text-[11px]", TONE_TEXT[stateInfo(r.trust_state).tone])}>
                          {r.trust_state}
                        </span>
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label>Profile</Label>
              <Select
                value={profile}
                onValueChange={setProfile}
                disabled={profiles.length === 0 || detailFetching}
              >
                <SelectTrigger className="w-full" aria-label="Profile">
                  <SelectValue
                    placeholder={detailFetching ? "loading…" : profiles.length === 0 ? "no profiles" : "select profile"}
                  />
                </SelectTrigger>
                <SelectContent>
                  {profiles.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          {recipeDigest ? (
            <div className="grid gap-2">
              <Label>Node overrides (optional — auto-placement when empty)</Label>
              <div className="grid gap-2 sm:grid-cols-2">
                {Array.from({ length: nodeCount }, (_, i) => (
                  <Select
                    key={i}
                    value={nodeOverrides[i] ?? ""}
                    onValueChange={(v) =>
                      setNodeOverrides((prev) => ({ ...prev, [i]: v === "" ? "" : v }))
                    }
                  >
                    <SelectTrigger className="w-full" aria-label={`Rank ${i} node`}>
                      <span className="flex items-center gap-2 text-xs">
                        <span className="font-mono text-muted">rank {i}</span>
                        <SelectValue placeholder="auto" />
                      </span>
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="">auto</SelectItem>
                      {(nodes ?? [])
                        .filter((n) => n.status === "online")
                        .map((n) => (
                          <SelectItem key={n.id} value={n.id}>
                            {n.display_name}
                          </SelectItem>
                        ))}
                    </SelectContent>
                  </Select>
                ))}
              </div>
            </div>
          ) : null}

          {plan ? (
            <div className="rounded border border-hairline p-3">
              <PlanPreview plan={plan} nodes={nodes ?? []} />
            </div>
          ) : planMutation.isError ? (
            <p className="rounded border border-fault/40 bg-fault/5 px-3 py-2 font-mono text-xs text-fault">
              {planMutation.error instanceof Error ? planMutation.error.message : "plan failed"}
            </p>
          ) : null}
        </div>

        <DialogFooter className="sm:justify-between">
          <Button
            variant="secondary"
            onClick={() => void preview()}
            disabled={!recipeDigest || !profile || planMutation.isPending}
          >
            {planMutation.isPending ? "planning…" : plan ? "Re-plan" : "Preview plan"}
          </Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button onClick={() => void create()} disabled={!plan?.ready || createMutation.isPending}>
              {createMutation.isPending ? "creating…" : "Create deployment"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
