import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { Network, Server } from "lucide-react";
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
  useCreateDeployment,
  useFabrics,
  useNodes,
  usePlanDeployment,
  useRecipe,
  useRecipes,
} from "~/lib/queries";
import { PlanPreview } from "~/components/plan-preview";
import { bytes } from "~/lib/format";

/**
 * Plan a deployment: pick an installed recipe + profile, optionally pin
 * nodes per rank, preview the plan, then create. The backend remains the
 * compatibility authority; node cards expose real inventory and fabrics.
 */
export function PlanDeploymentDialog({
  open,
  onOpenChange,
  initialRecipeDigest,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  initialRecipeDigest?: string;
}) {
  const navigate = useNavigate();
  const { data: recipes } = useRecipes();
  const { data: nodes } = useNodes();
  const fabricsQuery = useFabrics();
  const planMutation = usePlanDeployment();
  const createMutation = useCreateDeployment();
  const resetPlan = planMutation.reset;
  const resetCreate = createMutation.reset;
  const wasOpen = useRef(false);

  const [recipeDigest, setRecipeDigest] = useState("");
  const [profile, setProfile] = useState("");
  const [nodeOverrides, setNodeOverrides] = useState<Record<number, string>>({});
  const [variantChoices, setVariantChoices] = useState<Record<string, string>>({});
  const { data: recipeDetail, isFetching: detailFetching } = useRecipe(recipeDigest || undefined);

  const distinctRecipes = useMemo(() => {
    const seen = new Set<string>();
    const out = [] as (typeof recipes extends (infer T)[] | undefined ? T : unknown)[];
    for (const recipe of recipes ?? []) {
      const key = `${recipe.name}@${recipe.version}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(recipe);
    }
    return out;
  }, [recipes]);

  const profiles = useMemo(() => {
    const manifest = recipeDetail?.manifest;
    if (!manifest || typeof manifest !== "object") return [];
    const values = (manifest as Record<string, unknown>).profiles;
    if (!values || typeof values !== "object") return [];
    return Object.keys(values as Record<string, unknown>);
  }, [recipeDetail]);

  const variantArtifacts = useMemo(() => {
    const manifest = recipeDetail?.manifest as Record<string, unknown> | undefined;
    if (!manifest || !Array.isArray(manifest.artifacts)) return [];
    const out: { name: string; defaultVariant: string; variants: { name: string; label: string }[] }[] = [];
    for (const artifact of manifest.artifacts as Record<string, unknown>[]) {
      if (!Array.isArray(artifact.variants)) continue;
      out.push({
        name: String(artifact.name),
        defaultVariant: String(artifact.defaultVariant ?? ""),
        variants: (artifact.variants as Record<string, unknown>[]).map((variant) => ({
          name: String(variant.name),
          label: String(variant.label ?? variant.name),
        })),
      });
    }
    return out;
  }, [recipeDetail]);

  const nodeCount = recipeDetail?.compatibility?.nodeCount || 1;

  useEffect(() => {
    if (open && !wasOpen.current) {
      setRecipeDigest(initialRecipeDigest ?? "");
      setProfile("");
      setNodeOverrides({});
      setVariantChoices({});
      resetPlan();
      resetCreate();
    }
    wasOpen.current = open;
  }, [initialRecipeDigest, open, resetCreate, resetPlan]);

  useEffect(() => {
    setProfile((current) => (profiles.includes(current) ? current : profiles[0] ?? ""));
  }, [profiles]);

  const plan = planMutation.data;

  const preview = async () => {
    if (!recipeDigest) return;
    const placements = Object.entries(nodeOverrides)
      .filter(([, nodeId]) => nodeId)
      .map(([rank, node_id]) => ({ rank: Number(rank), node_id }));
    const variants = Object.fromEntries(
      Object.entries(variantChoices).filter(([, variant]) => variant),
    );
    try {
      await planMutation.mutateAsync({
        recipe_digest: recipeDigest,
        profile,
        ...(Object.keys(variants).length > 0 ? { variants } : {}),
        ...(placements.length > 0 ? { placements } : {}),
      });
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "plan failed");
    }
  };

  const create = async () => {
    if (!plan) return;
    const variants = Object.fromEntries(
      Object.entries(variantChoices).filter(([, variant]) => variant),
    );
    try {
      const deployment = await createMutation.mutateAsync({
        recipe_digest: recipeDigest,
        profile,
        plan_digest: plan.plan_digest,
        ...(Object.keys(variants).length > 0 ? { variants } : {}),
        ...(plan.placements.length > 0
          ? { placements: plan.placements.map((placement) => ({ node_id: placement.node_id, rank: placement.rank })) }
          : {}),
      });
      toast.success("Deployment launching", {
        description: `${deployment.recipe_name} @ ${deployment.profile}`,
      });
      onOpenChange(false);
      navigate(`/serving/deployments/${deployment.id}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "launch failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-4xl max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-h-[100dvh] max-sm:max-w-none max-sm:overflow-auto max-sm:rounded-none">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold">Launch deployment</DialogTitle>
          <DialogDescription>
            Select the recipe contract, optional profile and variants, then preview real placement before launch.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <section className="lmw-panel-raised grid gap-3 p-3 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label>Recipe</Label>
              <select
                aria-label="Recipe"
                className="h-8 w-full rounded-md border border-input bg-card px-2.5 text-sm outline-none focus-visible:border-ring"
                value={recipeDigest}
                onChange={(event) => {
                  setRecipeDigest(event.target.value);
                  setNodeOverrides({});
                  setVariantChoices({});
                  planMutation.reset();
                }}
              >
                <option value="">Select recipe</option>
                {distinctRecipes.map((recipe) => (
                  <option key={recipe.digest} value={recipe.digest}>
                    {recipe.display_name || recipe.name}@{recipe.version} ({recipe.trust_state})
                  </option>
                ))}
              </select>
            </div>
            <div className="grid gap-2">
              <Label>Profile</Label>
              <select
                aria-label="Profile"
                className="h-8 w-full rounded-md border border-input bg-card px-2.5 text-sm outline-none focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-50"
                value={profile}
                onChange={(event) => {
                  setProfile(event.target.value);
                  planMutation.reset();
                }}
                disabled={detailFetching}
              >
                <option value="">{profiles.length === 0 ? "No profiles" : "Select profile"}</option>
                {profiles.map((value) => <option key={value} value={value}>{value}</option>)}
              </select>
            </div>

            {variantArtifacts.map((artifact) => (
              <div key={artifact.name} className="grid gap-2">
                <Label>{artifact.name}</Label>
                <select
                  aria-label={artifact.name}
                  className="h-8 w-full rounded-md border border-input bg-card px-2.5 text-sm outline-none focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-50"
                  value={variantChoices[artifact.name] ?? artifact.defaultVariant}
                  onChange={(event) => {
                    setVariantChoices((current) => ({ ...current, [artifact.name]: event.target.value }));
                    planMutation.reset();
                  }}
                  disabled={detailFetching}
                >
                  {artifact.variants.map((variant) => <option key={variant.name} value={variant.name}>{variant.label}</option>)}
                </select>
              </div>
            ))}
          </section>

          {recipeDigest ? (
            <section className="grid gap-2">
              <div>
                <p className="lmw-label">Placement overrides</p>
                <p className="text-xs text-muted">Leave a rank on automatic placement to let the backend select compatible hardware.</p>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                {Array.from({ length: nodeCount }, (_, rank) => {
                  const selectedId = nodeOverrides[rank] ?? "";
                  const selectedNode = (nodes ?? []).find((node) => node.id === selectedId);
                  const memberships = (fabricsQuery.data ?? []).filter((fabric) => selectedId && fabric.members.includes(selectedId));
                  const accelerators = selectedNode?.inventory?.accelerators ?? [];
                  const rdmaCount = selectedNode?.inventory?.rdma_devices?.length ?? 0;
                  return (
                    <article key={rank} className="lmw-panel overflow-hidden">
                      <header className="lmw-panel-head flex items-center gap-2">
                        <Server className="h-4 w-4 text-primary" aria-hidden />
                        <h3 className="font-display font-semibold">Rank {rank}</h3>
                        <span className="ml-auto font-mono text-[10px] text-muted">{selectedNode?.status ?? "automatic"}</span>
                      </header>
                      <div className="grid gap-3 p-3">
                        <select
                          aria-label={`Rank ${rank} node`}
                          className="h-8 w-full rounded-md border border-input bg-card px-2.5 text-sm outline-none focus-visible:border-ring"
                          value={selectedId}
                          onChange={(event) => {
                            setNodeOverrides((current) => ({ ...current, [rank]: event.target.value }));
                            planMutation.reset();
                          }}
                        >
                          <option value="">Rank {rank} · automatic</option>
                          {(nodes ?? []).map((node) => (
                            <option key={node.id} value={node.id} disabled={node.status !== "online"}>
                              {node.display_name} · {node.status}{node.status !== "online" ? " · unavailable" : ""}
                            </option>
                          ))}
                        </select>

                        {selectedNode ? (
                          <dl className="grid grid-cols-2 gap-2 text-xs">
                            <div><dt className="lmw-label">Accelerators</dt><dd>{accelerators.length ? accelerators.map((accelerator) => accelerator.name).join(", ") : "None reported"}</dd></div>
                            <div><dt className="lmw-label">Accelerator memory</dt><dd>{accelerators.length ? bytes(accelerators.reduce((total, accelerator) => total + accelerator.memory_bytes, 0)) : "Not reported"}</dd></div>
                            <div><dt className="lmw-label">RDMA</dt><dd>{rdmaCount > 0 ? `${rdmaCount} device${rdmaCount === 1 ? "" : "s"}` : "Not present"}</dd></div>
                            <div>
                              <dt className="lmw-label">Fabrics</dt>
                              <dd className="space-y-0.5">
                                {fabricsQuery.isPending ? (
                                  <span className="text-muted">Loading fabric data…</span>
                                ) : fabricsQuery.isError ? (
                                  <span className="text-warn">Fabric data unavailable</span>
                                ) : memberships.length > 0 ? (
                                  memberships.map((fabric) => (
                                    <span key={fabric.id} className="flex items-center gap-1">
                                      <Network className="h-3 w-3 text-ok" aria-hidden />
                                      {fabric.name} · {fabric.transport}
                                    </span>
                                  ))
                                ) : (
                                  <span>None</span>
                                )}
                              </dd>
                            </div>
                          </dl>
                        ) : (
                          <p className="text-xs text-muted">Automatic placement uses live inventory, leases, fabric, and recipe compatibility.</p>
                        )}
                      </div>
                    </article>
                  );
                })}
              </div>
            </section>
          ) : null}

          {plan ? (
            <div className="rounded-md border border-hairline bg-card p-3">
              <PlanPreview plan={plan} nodes={nodes ?? []} />
            </div>
          ) : planMutation.isError ? (
            <p className="rounded-md border border-fault/40 bg-fault/5 px-3 py-2 font-mono text-xs text-fault">
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
            {planMutation.isPending ? "Planning…" : "Preview placement"}
          </Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button onClick={() => void create()} disabled={!plan?.ready || createMutation.isPending}>
              {createMutation.isPending ? "Launching…" : "Launch"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
