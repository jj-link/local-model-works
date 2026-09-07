import { useState } from "react";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { useImportRecipe } from "~/lib/queries";
import type { Recipe, RecipeSource } from "~/lib/api";
import { RecipeError } from "~/components/recipes/workflow";

type PackageSource = Exclude<RecipeSource["type"], "git">;
const sourceLabels: Record<PackageSource, string> = { catalog: "Catalog package", oci: "OCI package", local: "Local package directory" };
const placeholders: Record<PackageSource, string> = { catalog: "Catalog name or index URL", oci: "registry/repository@sha256:…", local: "/var/lib/local-model-works/recipes/my-recipe" };
/** Immediate import of an existing package; GitHub authoring lives on the recipe page. */
export function ImportRecipeDialog({ open, onOpenChange, onImported }: { open: boolean; onOpenChange: (open: boolean) => void; onImported: (recipe: Recipe) => void }) {
  const [type, setType] = useState<PackageSource>("catalog");
  const [inputs, setInputs] = useState<Record<PackageSource, string>>({ catalog: "", oci: "", local: "" });
  const mutation = useImportRecipe();
  const submit = async () => {
    const source: RecipeSource = type === "local" ? { type, path: inputs[type].trim() } : { type, reference: inputs[type].trim() };
    try { const recipe = await mutation.mutateAsync({ source }); onOpenChange(false); onImported(recipe); }
    catch { /* Preserve input; the typed failure is shown without an automatic retry. */ }
  };
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent className="sm:max-w-xl"><DialogHeader><DialogTitle>Import existing package</DialogTitle><DialogDescription>Import adds the existing package to the catalog immediately, then opens its recipe page. It does not download model files to devices or start a model. Use Add from GitHub for source inspection and editable review.</DialogDescription></DialogHeader>
    <label className="grid gap-2 text-sm">Package source<select aria-label="Package source" className="control bg-panel p-2" value={type} onChange={(event) => setType(event.target.value as PackageSource)}>{(Object.keys(sourceLabels) as PackageSource[]).map((value) => <option key={value} value={value}>{sourceLabels[value]}</option>)}</select></label>
    <label className="grid gap-2 text-sm">{type === "local" ? "Package directory on the server" : "Package reference"}<Input value={inputs[type]} onChange={(event) => setInputs((current) => ({ ...current, [type]: event.target.value }))} placeholder={placeholders[type]} /></label>
    {mutation.isError ? <RecipeError error={mutation.error} /> : null}
    <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button><Button disabled={!inputs[type].trim() || mutation.isPending} onClick={() => void submit()}>{mutation.isPending ? "Importing…" : "Import and add to catalog"}</Button></DialogFooter>
  </DialogContent></Dialog>;
}
