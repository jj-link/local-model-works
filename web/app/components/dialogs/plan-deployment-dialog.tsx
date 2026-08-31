import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { Download, HardDrive, Server } from "lucide-react";
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
  useCreateLaunchProfile,
  useDeleteLaunchProfile,
  useLaunchProfiles,
  useNodes,
  usePlanDeployment,
  useRecipe,
  useRecipes,
  useUpdateLaunchProfile,
} from "~/lib/queries";
import { bytes } from "~/lib/format";

type VariantChoice = {
  name: string;
  label: string;
  description: string;
};

type VariantArtifact = {
  name: string;
  label: string;
  defaultValue: string;
  variants: VariantChoice[];
};

type EnumParameter = {
  name: string;
  label: string;
  description: string;
  defaultValue: string;
  values: string[];
};

function titleCase(value: string) {
  return value
    .split("_")
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

export function PlanDeploymentDialog({
  open,
  onOpenChange,
  initialRecipeDigest,
  onBack,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  initialRecipeDigest?: string;
  onBack?: () => void;
}) {
  const navigate = useNavigate();
  const { data: recipes } = useRecipes();
  const { data: nodes } = useNodes();
  const planMutation = usePlanDeployment();
  const createMutation = useCreateDeployment();
  const createProfileMutation = useCreateLaunchProfile();
  const updateProfileMutation = useUpdateLaunchProfile();
  const deleteProfileMutation = useDeleteLaunchProfile();
  const resetPlan = planMutation.reset;
  const resetCreate = createMutation.reset;
  const wasOpen = useRef(false);
  const autoPreviewKey = useRef("");
  const initializedSettings = useRef("");

  const [recipeDigest, setRecipeDigest] = useState("");
  const [nodeOverrides, setNodeOverrides] = useState<Record<number, string>>({});
  const [variantChoices, setVariantChoices] = useState<Record<string, string>>({});
  const [parameterChoices, setParameterChoices] = useState<Record<string, unknown>>({});
  const [selectedProfileId, setSelectedProfileId] = useState("");
  const [sourceProfileId, setSourceProfileId] = useState("");
  const [newProfileName, setNewProfileName] = useState("");

  const { data: recipeDetail, isFetching: detailFetching } = useRecipe(recipeDigest || undefined);
  const profilesQuery = useLaunchProfiles(recipeDigest || undefined);

  const distinctRecipes = useMemo(() => {
    const seen = new Set<string>();
    const out = [] as NonNullable<typeof recipes>;
    for (const recipe of recipes ?? []) {
      const key = `${recipe.name}@${recipe.version}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(recipe);
    }
    return out;
  }, [recipes]);

  const manifest = useMemo(
    () => recipeDetail?.manifest as Record<string, unknown> | undefined,
    [recipeDetail],
  );

  const variantArtifacts = useMemo<VariantArtifact[]>(() => {
    if (!manifest || !Array.isArray(manifest.artifacts)) return [];
    const out: VariantArtifact[] = [];
    for (const value of manifest.artifacts as Record<string, unknown>[]) {
      if (!Array.isArray(value.variants) || value.variants.length === 0) continue;
      const name = String(value.name ?? "");
      const variants = (value.variants as Record<string, unknown>[]).map((variant) => ({
        name: String(variant.name ?? ""),
        label: String(variant.label ?? variant.name ?? ""),
        description: String(variant.description ?? ""),
      }));
      out.push({
        name,
        label: name === "model" ? "Model" : name === "drafter" ? "Drafter" : titleCase(name),
        defaultValue: String(value.defaultVariant ?? variants[0]?.name ?? ""),
        variants,
      });
    }
    return out;
  }, [manifest]);

  const enumParameters = useMemo<EnumParameter[]>(() => {
    if (!manifest || !Array.isArray(manifest.parameters)) return [];
    const out: EnumParameter[] = [];
    for (const value of manifest.parameters as Record<string, unknown>[]) {
      if (value.type !== "enum" || !Array.isArray(value.enum) || value.enum.length === 0) continue;
      const name = String(value.name ?? "");
      out.push({
        name,
        label: titleCase(name),
        description: String(value.description ?? ""),
        defaultValue: String(value.default ?? value.enum[0] ?? ""),
        values: value.enum.map(String),
      });
    }
    return out;
  }, [manifest]);

  const visibleVariantArtifacts = variantArtifacts.filter((artifact) => artifact.variants.length > 1);
  const visibleEnumParameters = enumParameters.filter((parameter) => parameter.values.length > 1);
  const profiles = profilesQuery.data ?? [];
  const sourceProfile = profiles.find((profile) => profile.id === sourceProfileId);
  const showSettings =
    visibleVariantArtifacts.length > 0 ||
    visibleEnumParameters.length > 0 ||
    profiles.length > 0;

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
    if (Object.entries(nodeOverrides).some(
      ([otherRank, nodeId]) => Number(otherRank) !== rank && nodeId === node.id,
    )) {
      return "already assigned";
    }
    if (!acceleratorRequirement) return "";
    const matching = (node.inventory?.accelerators ?? []).some((accelerator) => {
      const vendor = String(accelerator.vendor ?? "").toLowerCase();
      const architecture = String(accelerator.architecture ?? "");
      return (!acceleratorRequirement.vendor || vendor === acceleratorRequirement.vendor) &&
        (acceleratorRequirement.architectures.length === 0 ||
          acceleratorRequirement.architectures.includes(architecture)) &&
        (!acceleratorRequirement.minMemory ||
          Number(accelerator.memory_bytes ?? 0) >= acceleratorRequirement.minMemory);
    });
    return matching ? "" : "incompatible accelerator";
  };

  const resetForRecipe = (digest: string) => {
    setRecipeDigest(digest);
    setNodeOverrides({});
    setVariantChoices({});
    setParameterChoices({});
    setSelectedProfileId("");
    setSourceProfileId("");
    setNewProfileName("");
    initializedSettings.current = "";
    autoPreviewKey.current = "";
    resetPlan();
  };

  useEffect(() => {
    if (open && !wasOpen.current) {
      resetForRecipe(initialRecipeDigest ?? "");
      resetCreate();
    }
    wasOpen.current = open;
  }, [initialRecipeDigest, open, resetCreate, resetPlan]);

  useEffect(() => {
    if (!recipeDigest || !manifest || initializedSettings.current === recipeDigest) return;
    initializedSettings.current = recipeDigest;
    setVariantChoices(Object.fromEntries(
      variantArtifacts.map((artifact) => [artifact.name, artifact.defaultValue]),
    ));
    setParameterChoices(Object.fromEntries(
      enumParameters.map((parameter) => [parameter.name, parameter.defaultValue]),
    ));
  }, [enumParameters, manifest, recipeDigest, variantArtifacts]);

  const applyProfile = (profileId: string) => {
    if (!profileId) {
      setSelectedProfileId("");
      setSourceProfileId("");
      setVariantChoices(Object.fromEntries(
        variantArtifacts.map((artifact) => [artifact.name, artifact.defaultValue]),
      ));
      setParameterChoices(Object.fromEntries(
        enumParameters.map((parameter) => [parameter.name, parameter.defaultValue]),
      ));
      return;
    }
    const profile = profiles.find((candidate) => candidate.id === profileId);
    if (!profile) return;
    setSelectedProfileId(profile.id);
    setSourceProfileId(profile.id);
    setVariantChoices(Object.fromEntries(
      variantArtifacts.map((artifact) => [
        artifact.name,
        profile.variants?.[artifact.name] ?? artifact.defaultValue,
      ]),
    ));
    setParameterChoices(Object.fromEntries(
      enumParameters.map((parameter) => [
        parameter.name,
        profile.parameters?.[parameter.name] ?? parameter.defaultValue,
      ]),
    ));
    autoPreviewKey.current = "";
  };

  const markCustom = () => {
    setSelectedProfileId("");
    autoPreviewKey.current = "";
  };

  useEffect(() => {
    if (!open || !recipeDigest || !recipeDetail || !nodes || detailFetching) return;
    const placements = Object.entries(nodeOverrides)
      .filter(([, nodeId]) => nodeId)
      .map(([rank, node_id]) => ({ rank: Number(rank), node_id }));
    const request = selectedProfileId
      ? {
          recipe_digest: recipeDigest,
          launch_profile_id: selectedProfileId,
          ...(placements.length > 0 ? { placements } : {}),
        }
      : {
          recipe_digest: recipeDigest,
          variants: variantChoices,
          parameters: parameterChoices,
          ...(placements.length > 0 ? { placements } : {}),
        };
    const key = JSON.stringify(request);
    if (autoPreviewKey.current === key) return;
    autoPreviewKey.current = key;
    void planMutation.mutateAsync(request).catch(() => undefined);
  }, [
    detailFetching,
    nodeOverrides,
    nodes,
    open,
    parameterChoices,
    recipeDetail,
    recipeDigest,
    selectedProfileId,
    variantChoices,
  ]);

  const profileBody = () => ({
    name: newProfileName.trim() || sourceProfile?.name || "",
    variants: variantChoices,
    parameters: parameterChoices,
  });

  const saveProfile = async () => {
    if (!recipeDigest || !newProfileName.trim()) return;
    try {
      const profile = await createProfileMutation.mutateAsync({
        recipeDigest,
        body: profileBody(),
      });
      setNewProfileName("");
      setSelectedProfileId(profile.id);
      setSourceProfileId(profile.id);
      toast.success("Profile saved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "profile save failed");
    }
  };

  const updateProfile = async () => {
    if (!sourceProfile) return;
    try {
      const profile = await updateProfileMutation.mutateAsync({
        id: sourceProfile.id,
        body: { ...profileBody(), name: sourceProfile.name },
      });
      setSelectedProfileId(profile.id);
      setSourceProfileId(profile.id);
      toast.success("Profile updated");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "profile update failed");
    }
  };

  const deleteProfile = async () => {
    if (!sourceProfile) return;
    try {
      await deleteProfileMutation.mutateAsync({
        id: sourceProfile.id,
        recipeDigest,
      });
      applyProfile("");
      toast.success("Profile deleted");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "profile delete failed");
    }
  };

  const plan = planMutation.data;
  const originDownloads = plan?.transfers?.filter(
    (transfer) => transfer.action === "download-origin",
  ) ?? [];
  const peerCopies = plan?.transfers?.filter(
    (transfer) => transfer.action === "peer-copy",
  ) ?? [];
  const knownDownloadBytes = originDownloads.reduce(
    (total, transfer) => total + (transfer.bytes ?? 0),
    0,
  );
  const hasUnknownDownload = originDownloads.some((transfer) => transfer.bytes == null);
  const targetLabel = plan?.placements?.length
    ? plan.placements.map((placement) => placement.node_name ?? placement.node_id).join(", ")
    : "Checking compatible nodes…";
  const readonlyDiagnostic = plan?.diagnostics?.find(
    (diagnostic) => diagnostic.code === "storage.cache_root_readonly",
  );
  const readonlyNodeId = readonlyDiagnostic?.resource?.replace(/^node:/, "");
  const readonlyStorage = plan?.storage?.find(
    (storage) => storage.node_id === readonlyNodeId,
  );
  const endpoint = plan?.endpoint;
  const endpointLabel = endpoint?.host
    ? `${endpoint.host}:${endpoint.port ?? ""}${endpoint.path ?? ""}`
    : "Assigned at launch";
  const recipeTitle = recipeDetail?.name ??
    distinctRecipes.find((recipe) => recipe.digest === recipeDigest)?.name ??
    "deployment";

  const create = async () => {
    if (!plan) return;
    const request = selectedProfileId
      ? {
          recipe_digest: recipeDigest,
          launch_profile_id: selectedProfileId,
          plan_digest: plan.plan_digest,
          placements: plan.placements.map(({ node_id, rank }) => ({ node_id, rank })),
        }
      : {
          recipe_digest: recipeDigest,
          variants: variantChoices,
          parameters: parameterChoices,
          plan_digest: plan.plan_digest,
          placements: plan.placements.map(({ node_id, rank }) => ({ node_id, rank })),
        };
    try {
      const deployment = await createMutation.mutateAsync(request);
      toast.success("Deployment launching", {
        description: deployment.recipe_name ?? recipeTitle,
      });
      onOpenChange(false);
      navigate(`/serving/deployments/${deployment.id}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "launch failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold">
            Launch {recipeTitle}
          </DialogTitle>
          <DialogDescription className="sr-only">
            Confirm the target and launch this recipe.
          </DialogDescription>
        </DialogHeader>

        {!initialRecipeDigest ? (
          <div className="grid gap-2">
            <Label htmlFor="launch-recipe">Recipe</Label>
            <select
              id="launch-recipe"
              className="h-9 w-full rounded-md border border-input bg-card px-2.5 text-sm"
              value={recipeDigest}
              onChange={(event) => resetForRecipe(event.target.value)}
            >
              <option value="">Select recipe</option>
              {distinctRecipes.map((recipe) => (
                <option key={recipe.digest} value={recipe.digest}>
                  {recipe.name}@{recipe.version}
                </option>
              ))}
            </select>
          </div>
        ) : null}

        {recipeDigest ? (
          <div className="divide-y divide-hairline rounded-lg border border-hairline bg-card">
            <div className="grid grid-cols-[7rem_1fr] items-center gap-3 px-4 py-3">
              <span className="lmw-label">Target</span>
              {nodeCount === 1 ? (
                <select
                  aria-label="Target"
                  className="h-8 min-w-0 rounded-md border border-input bg-background px-2 text-sm"
                  value={nodeOverrides[0] ?? ""}
                  onChange={(event) => {
                    setNodeOverrides(event.target.value ? { 0: event.target.value } : {});
                    autoPreviewKey.current = "";
                  }}
                >
                  <option value="">{targetLabel}</option>
                  {(nodes ?? []).map((node) => {
                    const reason = nodeReason(node, 0);
                    return (
                      <option key={node.id} value={node.id} disabled={Boolean(reason)}>
                        {node.display_name}{reason ? ` · ${reason}` : ""}
                      </option>
                    );
                  })}
                </select>
              ) : (
                <span className="text-sm">{targetLabel}</span>
              )}
            </div>
            <div className="grid grid-cols-[7rem_1fr] items-center gap-3 px-4 py-3">
              <span className="lmw-label">Endpoint</span>
              <span className="font-mono text-xs">{endpointLabel}</span>
            </div>
            <div className="grid grid-cols-[7rem_1fr] items-center gap-3 px-4 py-3">
              <span className="lmw-label">Preparation</span>
              {planMutation.isPending || detailFetching ? (
                <span className="text-sm text-muted">Checking…</span>
              ) : originDownloads.length > 0 ? (
                <span className="flex items-center gap-2 text-sm text-warn">
                  <Download className="h-4 w-4" aria-hidden />
                  Download required
                  {hasUnknownDownload
                    ? " · size unknown"
                    : knownDownloadBytes > 0
                      ? ` · ${bytes(knownDownloadBytes)}`
                      : ""}
                </span>
              ) : peerCopies.length > 0 ? (
                <span className="flex items-center gap-2 text-sm">
                  <Server className="h-4 w-4 text-primary" aria-hidden />
                  Copy from another node
                </span>
              ) : (
                <span className="flex items-center gap-2 text-sm text-ok">
                  <HardDrive className="h-4 w-4" aria-hidden />
                  Installed
                </span>
              )}
            </div>
          </div>
        ) : null}

        {nodeCount > 1 && recipeDigest ? (
          <details className="rounded-lg border border-hairline bg-card" open>
            <summary className="cursor-pointer px-4 py-3 font-display text-sm font-semibold">
              Cluster roles
            </summary>
            <div className="grid gap-3 border-t border-hairline p-4 sm:grid-cols-2">
              {Array.from({ length: nodeCount }, (_, rank) => (
                <label key={rank} className="grid gap-1.5">
                  <span className="text-xs font-medium">
                    {rank === 0 ? "Head · API" : `Worker ${rank}`}
                  </span>
                  <select
                    aria-label={`Rank ${rank} node`}
                    className="h-8 rounded-md border border-input bg-background px-2 text-sm"
                    value={nodeOverrides[rank] ?? ""}
                    onChange={(event) => {
                      setNodeOverrides((current) => ({
                        ...current,
                        [rank]: event.target.value,
                      }));
                      autoPreviewKey.current = "";
                    }}
                  >
                    <option value="">Automatic</option>
                    {(nodes ?? []).map((node) => {
                      const reason = nodeReason(node, rank);
                      return (
                        <option key={node.id} value={node.id} disabled={Boolean(reason)}>
                          {node.display_name}{reason ? ` · ${reason}` : ""}
                        </option>
                      );
                    })}
                  </select>
                </label>
              ))}
            </div>
          </details>
        ) : null}

        {showSettings ? (
          <details className="rounded-lg border border-hairline bg-card">
            <summary className="cursor-pointer px-4 py-3 font-display text-sm font-semibold">
              Settings · {profiles.find((profile) => profile.id === selectedProfileId)?.name ?? "Custom"}
            </summary>
            <div className="grid gap-4 border-t border-hairline p-4">
              {profiles.length > 0 ? (
                <label className="grid gap-1.5">
                  <span className="text-xs font-medium">Profile</span>
                  <select
                    aria-label="Profile"
                    className="h-8 rounded-md border border-input bg-background px-2 text-sm"
                    value={selectedProfileId}
                    onChange={(event) => applyProfile(event.target.value)}
                  >
                    <option value="">Custom</option>
                    {profiles.map((profile) => (
                      <option key={profile.id} value={profile.id}>{profile.name}</option>
                    ))}
                  </select>
                </label>
              ) : null}

              {visibleVariantArtifacts.map((artifact) => (
                <fieldset key={artifact.name} className="grid gap-2">
                  <legend className="text-xs font-medium">{artifact.label}</legend>
                  <div className="grid gap-2 sm:grid-cols-2">
                    {artifact.variants.map((variant) => (
                      <label
                        key={variant.name}
                        className={`cursor-pointer rounded-md border p-3 ${
                          variantChoices[artifact.name] === variant.name
                            ? "border-primary bg-primary/5"
                            : "border-hairline"
                        }`}
                      >
                        <span className="flex gap-2">
                          <input
                            type="radio"
                            name={`variant-${artifact.name}`}
                            checked={variantChoices[artifact.name] === variant.name}
                            onChange={() => {
                              setVariantChoices((current) => ({
                                ...current,
                                [artifact.name]: variant.name,
                              }));
                              markCustom();
                            }}
                          />
                          <span>
                            <span className="block text-sm font-medium">{variant.label}</span>
                            {variant.description ? (
                              <span className="mt-1 block text-xs text-muted">{variant.description}</span>
                            ) : null}
                          </span>
                        </span>
                      </label>
                    ))}
                  </div>
                </fieldset>
              ))}

              {visibleEnumParameters.map((parameter) => (
                <label key={parameter.name} className="grid gap-1.5">
                  <span className="text-xs font-medium">{parameter.label}</span>
                  {parameter.description ? (
                    <span className="text-xs text-muted">{parameter.description}</span>
                  ) : null}
                  <select
                    aria-label={parameter.label}
                    className="h-8 rounded-md border border-input bg-background px-2 text-sm"
                    value={String(parameterChoices[parameter.name] ?? parameter.defaultValue)}
                    onChange={(event) => {
                      setParameterChoices((current) => ({
                        ...current,
                        [parameter.name]: event.target.value,
                      }));
                      markCustom();
                    }}
                  >
                    {parameter.values.map((value) => (
                      <option key={value} value={value}>{value}</option>
                    ))}
                  </select>
                </label>
              ))}

              <div className="flex flex-wrap items-end gap-2">
                <label className="grid min-w-44 flex-1 gap-1.5">
                  <span className="text-xs font-medium">New profile name</span>
                  <input
                    aria-label="New profile name"
                    className="h-8 rounded-md border border-input bg-background px-2 text-sm"
                    value={newProfileName}
                    onChange={(event) => setNewProfileName(event.target.value)}
                  />
                </label>
                <Button
                  size="sm"
                  variant="secondary"
                  disabled={!newProfileName.trim() || createProfileMutation.isPending}
                  onClick={() => void saveProfile()}
                >
                  Save profile
                </Button>
                {sourceProfile ? (
                  <>
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={updateProfileMutation.isPending}
                      onClick={() => void updateProfile()}
                    >
                      Update profile
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={deleteProfileMutation.isPending}
                      onClick={() => void deleteProfile()}
                    >
                      Delete profile
                    </Button>
                  </>
                ) : null}
              </div>
            </div>
          </details>
        ) : null}

        {planMutation.isError ? (
          <p className="rounded-md border border-fault/40 bg-fault/5 px-3 py-2 text-sm text-fault">
            {planMutation.error instanceof Error ? planMutation.error.message : "Unable to plan launch"}
          </p>
        ) : null}
        {plan && !plan.ready && !planMutation.isPending ? (
          <div className="rounded-md border border-warn/40 bg-warn/5 px-3 py-2 text-sm text-warn">
            <p>
              {plan.diagnostics?.[0]?.message ?? "No compatible launch target is ready."}
              {recipeDetail?.compatibility?.fabric && !plan.fabric ? (
                <> <Link to="/fleet/fabrics" className="underline underline-offset-2">Open Fabrics</Link></>
              ) : null}
            </p>
            {readonlyNodeId && readonlyStorage?.cache_root ? (
              <p className="mt-2 text-xs">
                Add <code className="font-mono">ReadWritePaths={readonlyStorage.cache_root}</code> to a
                <code className="mx-1 font-mono">local-model-works-agent.service.d</code> drop-in, run
                <code className="mx-1 font-mono">sudo systemctl daemon-reload &amp;&amp; sudo systemctl restart local-model-works-agent</code>,
                then re-plan. <Link to={`/fleet/nodes/${readonlyNodeId}`} className="underline underline-offset-2">Node details</Link>
              </p>
            ) : null}
          </div>
        ) : null}

        <DialogFooter className="sm:justify-between">
          <div>
            {onBack ? (
              <Button variant="ghost" onClick={onBack}>Back</Button>
            ) : null}
          </div>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button
              onClick={() => void create()}
              disabled={!plan?.ready || createMutation.isPending || planMutation.isPending}
            >
              {createMutation.isPending
                ? "Launching…"
                : originDownloads.length > 0
                  ? "Download & launch"
                  : "Launch"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
