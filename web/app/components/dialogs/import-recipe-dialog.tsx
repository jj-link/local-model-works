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
import {
  useImportRecipe,
  useRecipe,
} from "~/lib/queries";
import type { Recipe, RecipeSource } from "~/lib/api";
import { bytes, shortDigest } from "~/lib/format";

const SOURCE_TYPES = [
  { value: "git", label: "GitHub repository", hint: "recommended" },
  { value: "catalog", label: "Catalog", hint: "catalog entry" },
  { value: "oci", label: "OCI package", hint: "immutable digest" },
  { value: "local", label: "Local directory", hint: "controller path" },
] as const;

type Stage = "source" | "review" | "ready";

function StepRail({ stage }: { stage: Stage }) {
  const stages: { id: Stage; label: string }[] = [
    { id: "source", label: "Source" },
    { id: "review", label: "Review" },
    { id: "ready", label: "Ready" },
  ];
  const current = stages.findIndex((item) => item.id === stage);
  return (
    <ol className="grid grid-cols-3 gap-2" aria-label="Recipe import progress">
      {stages.map((item, index) => (
        <li
          key={item.id}
          className={`rounded border px-3 py-2 ${
            index <= current
              ? "border-primary/50 bg-primary/5 text-foreground"
              : "border-hairline bg-raised text-muted"
          }`}
          aria-current={item.id === stage ? "step" : undefined}
        >
          <span className="block font-mono text-[10px] text-muted">0{index + 1}</span>
          <span className="font-display text-xs font-semibold">{item.label}</span>
        </li>
      ))}
    </ol>
  );
}

function objectRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" ? value as Record<string, unknown> : undefined;
}

/** Repository-first import with immutable contract review. */
export function ImportRecipeDialog({
  open,
  onOpenChange,
  onPlan,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onPlan?: (recipe: Recipe) => void;
}) {
  const navigate = useNavigate();
  const importRecipe = useImportRecipe();
  const [stage, setStage] = useState<Stage>("source");
  const [type, setType] = useState<RecipeSource["type"]>("git");
  const [reference, setReference] = useState("");
  const [revision, setRevision] = useState("");
  const [localPath, setLocalPath] = useState("");
  const [installed, setInstalled] = useState<Recipe>();
  const detail = useRecipe(installed?.digest);

  useEffect(() => {
    if (!open) return;
    setStage("source");
    setType("git");
    setReference("");
    setRevision("");
    setLocalPath("");
    setInstalled(undefined);
    importRecipe.reset();
  }, [open]);

  const manifest = objectRecord(detail.data?.manifest);
  const artifacts = useMemo(() => {
    if (!Array.isArray(manifest?.artifacts)) return [];
    return manifest.artifacts.map((raw) => {
      const artifact = objectRecord(raw) ?? {};
      const variants = Array.isArray(artifact.variants)
        ? artifact.variants.map((rawVariant) => {
            const variant = objectRecord(rawVariant) ?? {};
            const source = objectRecord(variant.source);
            return {
              name: String(variant.name ?? ""),
              label: String(variant.label ?? variant.name ?? "variant"),
              description: String(variant.description ?? ""),
              identity: String(source?.identity ?? "source not reported"),
              revision: String(source?.revision ?? ""),
            };
          })
        : [];
      if (variants.length > 0) {
        return {
          name: String(artifact.name ?? "artifact"),
          size: Number(artifact.sizeBytes ?? 0),
          defaultVariant: String(artifact.defaultVariant ?? ""),
          variants,
        };
      }
      const source = objectRecord(artifact.source);
      return {
        name: String(artifact.name ?? "artifact"),
        size: Number(artifact.sizeBytes ?? 0),
        defaultVariant: "",
        variants: [{
          name: "",
          label: "",
          description: "",
          identity: String(source?.identity ?? "source not reported"),
          revision: String(source?.revision ?? ""),
        }],
      };
    });
  }, [manifest]);
  const workload = Array.isArray(manifest?.workloads)
    ? objectRecord(manifest.workloads[0])
    : undefined;
  const image = objectRecord(workload?.image);
  const compatibility = objectRecord(manifest?.compatibility);
  const fabric = objectRecord(compatibility?.fabric);
  const pinnedRevision = installed?.source?.revision;

  const submit = async () => {
    const source: RecipeSource =
      type === "local"
        ? { type, path: localPath.trim() }
        : type === "git"
          ? {
              type,
              remote: reference.trim(),
              ...(revision.trim() ? { revision: revision.trim() } : {}),
            }
          : { type, reference: reference.trim() };
    try {
      const recipe = await importRecipe.mutateAsync({ source });
      setInstalled(recipe);
      setStage("ready");
      toast.success("Repository compiled and pinned", {
        description: `${recipe.name}@${recipe.version}`,
      });
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "Recipe import failed");
    }
  };


  const plan = () => {
    if (!installed) return;
    onOpenChange(false);
    if (onPlan) {
      onPlan(installed);
      return;
    }
    navigate(`/library/recipes/${installed.digest}?launch=1`);
  };

  const sourcePlaceholder: Record<RecipeSource["type"], string> = {
    git: "https://github.com/org/model-recipe",
    catalog: "catalog name or index URL",
    oci: "registry/repository@sha256:…",
    local: "/var/lib/local-model-works/recipes/my-recipe",
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-h-[100dvh] max-sm:max-w-none max-sm:overflow-auto max-sm:rounded-none">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold tracking-wide">
            Add a model recipe
          </DialogTitle>
          <DialogDescription>
            LMW turns a source repository into one reviewed, immutable launch contract.
          </DialogDescription>
        </DialogHeader>

        <StepRail stage={stage} />

        {stage === "source" ? (
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label>Source</Label>
              <Select value={type} onValueChange={(value) => setType(value as RecipeSource["type"])}>
                <SelectTrigger className="w-full" aria-label="Recipe source">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {SOURCE_TYPES.map((source) => (
                    <SelectItem key={source.value} value={source.value}>
                      <span className="flex items-baseline gap-2">
                        <span>{source.label}</span>
                        <span className="text-[11px] text-muted-foreground">{source.hint}</span>
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="grid gap-2">
              <Label htmlFor="import-ref">
                {type === "git" ? "Repository URL" : type === "local" ? "Directory" : "Reference"}
              </Label>
              <Input
                id="import-ref"
                value={type === "local" ? localPath : reference}
                onChange={(event) => type === "local"
                  ? setLocalPath(event.target.value)
                  : setReference(event.target.value)}
                placeholder={sourcePlaceholder[type]}
                className="font-mono text-xs"
                spellCheck={false}
                autoFocus
              />
            </div>

            {type === "git" ? (
              <div className="grid gap-2 rounded border border-hairline bg-raised p-3">
                <Label htmlFor="import-rev">Exact commit <span className="text-muted">(optional)</span></Label>
                <Input
                  id="import-rev"
                  value={revision}
                  onChange={(event) => setRevision(event.target.value)}
                  placeholder="Leave empty to pin the latest default-branch commit"
                  className="font-mono text-xs"
                  spellCheck={false}
                />
                <p className="text-xs text-muted">
                  LMW resolves “latest” once, records the full commit, and never follows a moving branch at launch.
                </p>
              </div>
            ) : null}

            {importRecipe.isError ? (
              <div className="rounded border border-fault/40 bg-fault/5 px-3 py-2" role="alert">
                <p className="font-display text-sm font-semibold text-fault">Could not add this source</p>
                <p className="mt-1 font-mono text-xs text-fault">
                  {importRecipe.error instanceof Error ? importRecipe.error.message : "Recipe import failed"}
                </p>
                <p className="mt-1 text-xs text-muted">
                  Check that the repository is reachable and supported, then retry. No partial recipe is activated.
                </p>
              </div>
            ) : null}
          </div>
        ) : (
          <div className="grid gap-4">
            <section className="rounded border border-hairline bg-raised p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <p className="lmw-label">Launch contract ready</p>
                  <h3 className="mt-1 font-display text-lg font-semibold">
                    {installed?.name}
                  </h3>
                  <p className="mt-1 text-sm text-muted">{installed?.description}</p>
                </div>
                <span className="rounded border border-ok/40 px-2 py-1 font-mono text-[11px] text-ok">
                  compiled · launchable
                </span>
              </div>
              <dl className="mt-4 grid gap-3 text-xs sm:grid-cols-3">
                <div><dt className="lmw-label">Source commit</dt><dd className="mt-1 font-mono">{pinnedRevision ? shortDigest(pinnedRevision) : "resolving…"}</dd></div>
                <div><dt className="lmw-label">License</dt><dd className="mt-1">{installed?.license || "Not reported"}</dd></div>
                <div><dt className="lmw-label">Hardware</dt><dd className="mt-1">{String(compatibility?.nodeCount ?? "—")} nodes · {String(fabric?.transport ?? "no fabric")}</dd></div>
              </dl>
            </section>

            <section className="grid gap-2">
              <p className="lmw-label">Pinned runtime</p>
              <div className="rounded border border-hairline">
                <div className="border-b border-hairline px-3 py-2">
                  <span className="text-xs text-muted">image</span>
                  <p className="truncate font-mono text-[11px]" title={String(image?.reference ?? "")}>
                    {String(image?.reference ?? (detail.isFetching ? "loading…" : "not reported"))}
                  </p>
                </div>
                {artifacts.flatMap((artifact) =>
                  artifact.variants.map((variant) => (
                    <div key={`${artifact.name}-${variant.name}`} className="grid gap-1 border-b border-hairline px-3 py-2 last:border-b-0 sm:grid-cols-[9rem_1fr_auto] sm:items-center">
                      <span className="font-display text-xs font-semibold">
                        {variant.label || artifact.name}
                        {variant.name && variant.name === artifact.defaultVariant ? (
                          <span className="ml-1 font-mono text-[9px] font-normal text-ok">default</span>
                        ) : null}
                      </span>
                      <span className="min-w-0">
                        <span className="block truncate font-mono text-[11px]" title={variant.identity}>
                          {variant.identity}{variant.revision ? ` @ ${shortDigest(variant.revision)}` : ""}
                        </span>
                        {variant.description ? <span className="mt-0.5 block text-[10px] text-muted">{variant.description}</span> : null}
                      </span>
                      <span className="font-mono text-[11px] text-muted">{artifact.size > 0 ? bytes(artifact.size) : "size unknown"}</span>
                    </div>
                  )),
                )}
              </div>
            </section>

            <div className="rounded border border-hairline bg-raised px-3 py-2 text-sm">
              <p>
                Permissions: {installed?.permissions?.join(", ") || "standard container access"}
                {installed?.high_risk?.length ? ` · elevated: ${installed.high_risk.join(", ")}` : ""}.
              </p>
              <p className="mt-1 text-xs text-muted">
                Next, LMW will match {String(compatibility?.nodeCount ?? "the required")} compatible nodes, validate fabric and host readiness, show cache/download work, and ask once before launch.
              </p>
            </div>
          </div>
        )}

        <DialogFooter className="sm:justify-between">
          <div>
            {installed ? (
              <Button
                variant="ghost"
                onClick={() => {
                  onOpenChange(false);
                  navigate(`/library/recipes/${installed.digest}`);
                }}
              >
                View full manifest
              </Button>
            ) : null}
          </div>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              {stage === "source" ? "Cancel" : "Close"}
            </Button>
            {stage === "source" ? (
              <Button
                onClick={() => void submit()}
                disabled={
                  importRecipe.isPending ||
                  (type === "local" ? !localPath.trim() : !reference.trim())
                }
              >
                {importRecipe.isPending ? "Resolving & compiling…" : "Resolve & review"}
              </Button>
            ) : (
              <Button onClick={plan}>Plan launch</Button>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
