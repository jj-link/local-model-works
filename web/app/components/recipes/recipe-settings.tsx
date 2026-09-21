import { Button } from "~/components/ui/button";
import type { RecipeDetail } from "~/lib/api";
import { RuntimeSelections, type RuntimeSelection } from "./launch-options";
import { record, records } from "./workflow";

export function RecipeSettings({ recipe, parameters, selection, onParametersChange, onSelectionChange, onReset, onRun }: {
  recipe: RecipeDetail;
  parameters: Record<string, unknown>;
  selection: RuntimeSelection;
  onParametersChange: (parameters: Record<string, unknown>) => void;
  onSelectionChange: (selection: RuntimeSelection) => void;
  onReset: () => void;
  onRun: () => void;
}) {
  const manifest = record(recipe.manifest);
  const parameterFields = records(manifest.parameters);
  const artifacts = records(manifest.artifacts).filter((artifact) => Array.isArray(artifact.variants) && artifact.variants.length);
  const hasSettings = parameterFields.length > 0 || artifacts.length > 0 || records(manifest.workloads).length > 1;

  return <section className="space-y-4">
    <div className="space-y-1">
      <h2 className="font-display text-lg font-semibold">Run settings</h2>
      <p className="text-sm text-muted">These values apply to your next run. They do not change the saved recipe or running deployments.</p>
    </div>
    <form noValidate className="space-y-4" onSubmit={(event) => { event.preventDefault(); onRun(); }}>
      {!hasSettings ? <p className="text-sm text-muted">This recipe has no configurable run settings. Choose a device and review the run to continue.</p> : null}
      <RuntimeSelections manifest={manifest} value={selection} onChange={onSelectionChange} parameters={parameters} onParametersChange={onParametersChange} compact />
      <div className="flex flex-wrap gap-2">
        <Button type="button" variant="outline" onClick={onReset}>Reset to recipe defaults</Button>
        <Button type="submit">Run</Button>
      </div>
    </form>
  </section>;
}
