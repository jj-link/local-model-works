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
import { useCreateDeployment, useNodes, usePlanDeployment, useRecipe, useRecipes } from "~/lib/queries";
import { PlanPreview } from "~/components/plan-preview";

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
  const resetPlan = planMutation.reset;
  const resetCreate = createMutation.reset;

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
    ((recipeDetail?.compatibility as { nodeCount?: number } | undefined)?.nodeCount ?? 1) || 1;

  useEffect(() => {
    if (open) {
      setRecipeDigest("");
      setProfile("");
      setNodeOverrides({});
      resetPlan();
      resetCreate();
    }
  }, [open, resetPlan, resetCreate]);

  useEffect(() => {
    setProfile((p) => (profiles.includes(p) ? p : profiles[0] ?? ""));
  }, [profiles]);

  const plan = planMutation.data;

  const preview = async () => {
    if (!recipeDigest) return;
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
            <div className="grid gap-2">
              <Label>Recipe</Label>
              <select
                aria-label="Recipe"
                className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring [color-scheme:dark]"
                value={recipeDigest}
                onChange={(e) => {
                  setRecipeDigest(e.target.value);
                  setNodeOverrides({});
                  planMutation.reset();
                }}
              >
                <option value="">select recipe</option>
                {(recipes ?? []).map((r) => (
                  <option key={r.digest} value={r.digest}>
                    {r.name}@{r.version} ({r.trust_state})
                  </option>
                ))}
              </select>
            </div>
            <div className="grid gap-2">
              <Label>Profile</Label>
              <select
                aria-label="Profile"
                className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-50 [color-scheme:dark]"
                value={profile}
                onChange={(e) => setProfile(e.target.value)}
                disabled={detailFetching}
              >
                <option value="">{profiles.length === 0 ? "no profiles" : "select profile"}</option>
                {profiles.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </select>
            </div>

          {recipeDigest ? (
            <div className="grid gap-2">
              <Label>Node overrides (optional — auto-placement when empty)</Label>
              <div className="grid gap-2 sm:grid-cols-2">
                {Array.from({ length: nodeCount }, (_, i) => (
                  <select
                    key={i}
                    aria-label={`Rank ${i} node`}
                    className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring [color-scheme:dark]"
                    value={nodeOverrides[i] ?? ""}
                    onChange={(e) =>
                      setNodeOverrides((prev) => ({ ...prev, [i]: e.target.value }))
                    }
                  >
                    <option value="">rank {i} · auto</option>
                    {(nodes ?? [])
                      .filter((n) => n.status === "online")
                      .map((n) => (
                        <option key={n.id} value={n.id}>
                          rank {i} · {n.display_name}
                        </option>
                      ))}
                  </select>
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
            disabled={!recipeDigest || planMutation.isPending}
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
