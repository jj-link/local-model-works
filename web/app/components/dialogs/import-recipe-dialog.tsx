import { useEffect, useState } from "react";
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
import { useImportRecipe } from "~/lib/queries";
import type { RecipeSource } from "~/lib/api";

const SOURCE_TYPES = [
  { value: "catalog", label: "catalog", hint: "signed catalog index entry" },
  { value: "oci", label: "oci", hint: "immutable digest reference" },
  { value: "git", label: "git", hint: "pinned full commit" },
  { value: "local", label: "local", hint: "operator directory" },
] as const;

/**
 * Install a recipe from catalog / OCI / pinned Git / local path. The
 * source schema follows the library fragment's RecipeSource shape.
 */
export function ImportRecipeDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const importRecipe = useImportRecipe();
  const [type, setType] = useState<RecipeSource["type"]>("catalog");
  const [reference, setReference] = useState("");
  const [revision, setRevision] = useState("");
  const [localPath, setLocalPath] = useState("");

  useEffect(() => {
    if (open) {
      setType("catalog");
      setReference("");
      setRevision("");
      setLocalPath("");
    }
  }, [open]);

  const referencePlaceholder: Record<RecipeSource["type"], string> = {
    catalog: "catalog name or index URL",
    oci: "registry/repo@sha256:…",
    git: "https://github.com/org/recipe-repo",
    local: "/var/lib/local-model-works/recipes/my-recipe",
  };

  const onSubmit = async () => {
    const source: RecipeSource =
      type === "local"
        ? { type, path: localPath }
        : type === "git"
          ? { type, remote: reference, revision }
          : { type, reference };
    try {
      const r = await importRecipe.mutateAsync({ source });
      toast.success("Recipe installed", {
        description: `${r.name}@${r.version} (${r.trust_state})`,
      });
      onOpenChange(false);
      navigate(`/library/recipes/${r.digest}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "import failed", {
        description: e instanceof Error ? undefined : "recipe.import_failed",
      });
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg max-sm:max-h-[92dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Install recipe
          </DialogTitle>
          <DialogDescription>
            Packages are content-addressed. Installs verify signatures where available; anything
            unverified lands as <span className="text-primary">untrusted</span> until you review
            it.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>Source type</Label>
            <Select value={type} onValueChange={(v) => setType(v as RecipeSource["type"])}>
              <SelectTrigger className="w-full" aria-label="Source type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SOURCE_TYPES.map((s) => (
                  <SelectItem key={s.value} value={s.value}>
                    <span className="flex items-baseline gap-2">
                      <span>{s.label}</span>
                      <span className="text-[11px] text-muted-foreground">{s.hint}</span>
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {type !== "local" ? (
            <div className="grid gap-2">
              <Label htmlFor="import-ref">{type === "git" ? "Remote" : "Reference"}</Label>
              <Input
                id="import-ref"
                value={reference}
                onChange={(e) => setReference(e.target.value)}
                placeholder={referencePlaceholder[type]}
                className="font-mono text-xs"
                spellCheck={false}
              />
            </div>
          ) : null}

          {type === "git" ? (
            <div className="grid gap-2">
              <Label htmlFor="import-rev">Revision (40-hex commit)</Label>
              <Input
                id="import-rev"
                value={revision}
                onChange={(e) => setRevision(e.target.value)}
                placeholder="6c47916f85e52b5e712223ca8f93952f90255714"
                className="font-mono text-xs"
                spellCheck={false}
              />
            </div>
          ) : null}

          {type === "local" ? (
            <div className="grid gap-2">
              <Label htmlFor="import-path">Path</Label>
              <Input
                id="import-path"
                value={localPath}
                onChange={(e) => setLocalPath(e.target.value)}
                placeholder={referencePlaceholder.local}
                className="font-mono text-xs"
                spellCheck={false}
              />
            </div>
          ) : null}

          <p className="rounded border border-hairline bg-raised px-3 py-2 font-mono text-[11px] leading-relaxed text-muted">
            trust: catalog → verified (signature) · git/local → untrusted until marked local ·
            oci → verified (digest)
          </p>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={
              importRecipe.isPending ||
              (type === "git"
                ? !reference || !revision
                : type === "local"
                  ? !localPath
                  : !reference)
            }
          >
            {importRecipe.isPending ? "installing…" : "Install"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
