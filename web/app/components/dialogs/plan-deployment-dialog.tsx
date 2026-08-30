import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
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
  const autoPreviewKey = useRef("");

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
    const out: { name: string; defaultVariant: string; sizeBytes: number; variants: { name: string; label: string; description: string }[] }[] = [];
    for (const artifact of manifest.artifacts as Record<string, unknown>[]) {
      if (!Array.isArray(artifact.variants)) continue;
      out.push({
        name: String(artifact.name),
        defaultVariant: String(artifact.defaultVariant ?? ""),
        sizeBytes: Number(artifact.sizeBytes ?? 0),
        variants: (artifact.variants as Record<string, unknown>[]).map((variant) => ({
          name: String(variant.name),
          label: String(variant.label ?? variant.name),
          description: String(variant.description ?? ""),
        })),
      });
    }
    return out;
  }, [recipeDetail]);

  const nodeCount = recipeDetail?.compatibility?.nodeCount || 1;

  const acceleratorRequirement = useMemo(() => {
    const accelerator = recipeDetail?.compatibility?.accelerator;
    if (!accelerator || typeof accelerator !== "object") return undefined;
    const value = accelerator as Record<string, unknown>;
    return {
      vendor: String(value.vendor ?? "").toLowerCase(),
      architectures: Array.isArray(value.architectures) ? value.architectures.map(String) : [],
      minMemory: Number(value.minMemoryBytes ?? 0),
    };
  }, [recipeDetail]);

  const nodeReason = (node: NonNullable<typeof nodes>[number], rank: number) => {
    if (node.status !== "online") return node.status;
    if (Object.entries(nodeOverrides).some(([otherRank, nodeId]) => Number(otherRank) !== rank && nodeId === node.id)) {
      return "already assigned";
    }
    if (!acceleratorRequirement) return "";
    const matching = (node.inventory?.accelerators ?? []).some((accelerator) => {
      const vendor = String(accelerator.vendor ?? "").toLowerCase();
      const architecture = String(accelerator.architecture ?? "");
      return (!acceleratorRequirement.vendor || vendor === acceleratorRequirement.vendor) &&
        (acceleratorRequirement.architectures.length === 0 || acceleratorRequirement.architectures.includes(architecture)) &&
        (!acceleratorRequirement.minMemory || Number(accelerator.memory_bytes ?? 0) >= acceleratorRequirement.minMemory);
    });
    return matching ? "" : "incompatible accelerator";
  };

  useEffect(() => {
    if (open && !wasOpen.current) {
      setRecipeDigest(initialRecipeDigest ?? "");
      setProfile("");
      setNodeOverrides({});
      setVariantChoices({});
      resetPlan();
      resetCreate();
      autoPreviewKey.current = "";
    }
    wasOpen.current = open;
  }, [initialRecipeDigest, open, resetCreate, resetPlan]);

  useEffect(() => {
    setProfile((current) => (profiles.includes(current) ? current : profiles[0] ?? ""));
  }, [profiles]);

  const plan = planMutation.data;
  const selectedFabric = plan?.fabric
    ? (fabricsQuery.data ?? []).find((fabric) => fabric.id === plan.fabric)
    : undefined;

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

  useEffect(() => {
    if (!open || !initialRecipeDigest || !recipeDetail || !nodes || detailFetching) return;
    const key = `${recipeDigest}:${profile}:${nodes.map((node) => `${node.id}:${node.status}`).join(",")}`;
    if (autoPreviewKey.current === key) return;
    autoPreviewKey.current = key;
    void preview();
  }, [open, initialRecipeDigest, recipeDetail, nodes, detailFetching, recipeDigest, profile]);

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
      <DialogContent className="sm:max-h-[92dvh] sm:max-w-4xl sm:overflow-auto max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-h-[100dvh] max-sm:max-w-none max-sm:overflow-auto max-sm:rounded-none">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold">Launch deployment</DialogTitle>
          <DialogDescription>
            Confirm the launch contract, assign Head and Worker roles, then preview capacity, fabric wiring, downloads, and endpoint before anything changes.
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

            {variantArtifacts.map((artifact) => {
              const selected = variantChoices[artifact.name] ?? artifact.defaultVariant;
              const label = artifact.name === "model" ? "Model weights" : artifact.name;
              return (
                <fieldset key={artifact.name} className="grid gap-2 sm:col-span-2">
                  <legend className="text-sm font-medium">{label}</legend>
                  <p className="text-xs text-muted">
                    Choose once for this deployment. Both ranks receive the same immutable snapshot
                    {artifact.sizeBytes > 0 ? ` · ${bytes(artifact.sizeBytes)} per node` : ""}.
                  </p>
                  <div className="grid gap-2 sm:grid-cols-2">
                    {artifact.variants.map((variant) => (
                      <label
                        key={variant.name}
                        className={`cursor-pointer rounded border p-3 transition-colors ${
                          selected === variant.name
                            ? "border-primary bg-primary/5"
                            : "border-hairline bg-card hover:border-primary/50"
                        }`}
                      >
                        <span className="flex items-start gap-2">
                          <input
                            type="radio"
                            name={`variant-${artifact.name}`}
                            value={variant.name}
                            checked={selected === variant.name}
                            onChange={() => {
                              setVariantChoices((current) => ({ ...current, [artifact.name]: variant.name }));
                              planMutation.reset();
                            }}
                            disabled={detailFetching}
                            className="mt-0.5 h-4 w-4 accent-[var(--color-primary)]"
                          />
                          <span>
                            <span className="block font-display text-sm font-semibold">{variant.label}</span>
                            {variant.description ? <span className="mt-1 block text-xs text-muted">{variant.description}</span> : null}
                          </span>
                        </span>
                      </label>
                    ))}
                  </div>
                </fieldset>
              );
            })}
          </section>

          {recipeDigest ? (
            <section className="grid gap-2">
              <div>
                <p className="lmw-label">Cluster roles</p>
                <p className="text-xs text-muted">Automatic placement uses compatible online hardware. Pin a role only when the cluster order matters.</p>
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
                        <h3 className="font-display font-semibold">{rank === 0 ? "Head · API" : `Worker ${rank}`}</h3>
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
                          <option value="">{rank === 0 ? "Head · automatic" : `Worker ${rank} · automatic`}</option>
                          {(nodes ?? []).map((node) => {
                            const reason = nodeReason(node, rank);
                            return (
                              <option key={node.id} value={node.id} disabled={Boolean(reason)}>
                                {node.display_name} · {reason || "compatible"}
                              </option>
                            );
                          })}
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
                          <p className="text-xs text-muted">LMW will match this role against live accelerator inventory, leases, and a complete shared fabric.</p>
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
              <PlanPreview plan={plan} nodes={nodes ?? []} fabric={selectedFabric} />
            </div>
          ) : planMutation.isError ? (
            <p className="rounded-md border border-fault/40 bg-fault/5 px-3 py-2 font-mono text-xs text-fault">
              {planMutation.error instanceof Error ? planMutation.error.message : "plan failed"}
            </p>
          ) : null}
          {recipeDigest && recipeDetail?.compatibility?.fabric && !plan?.fabric && !planMutation.isPending ? (
            <p className="rounded border border-warn/40 bg-warn/5 px-3 py-2 text-xs text-warn">
              This recipe needs a healthy cluster fabric with complete per-node wiring.{" "}
              <Link to="/fleet/fabrics" className="underline underline-offset-2">Open Fabrics</Link>
            </p>
          ) : null}
        </div>

        <DialogFooter className="sm:justify-between">
          <Button
            variant="secondary"
            onClick={() => void preview()}
            disabled={!recipeDigest || planMutation.isPending}
          >
            {planMutation.isPending ? "Checking cluster…" : plan ? "Recheck placement" : "Preview placement"}
          </Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button onClick={() => void create()} disabled={!plan?.ready || createMutation.isPending}>
              {createMutation.isPending
                ? "Launching…"
                : plan?.transfers?.some((transfer) => transfer.source_node === "origin")
                  ? "Download & launch"
                  : "Launch now"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
