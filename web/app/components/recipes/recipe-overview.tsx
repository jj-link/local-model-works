import { Link } from "react-router";
import { StatusDot } from "~/components/status-dot";
import { Button } from "~/components/ui/button";
import type * as api from "~/lib/api";
import { useDeployments, useNodes } from "~/lib/queries";
import { isCurrentDeployment, isLiveNode } from "~/lib/telemetry";
import { RecipeError } from "./workflow";

type RecipeOverviewProps = {
  recipe: api.RecipeDetail;
  repository?: api.RecipeRepositoryDetail;
  onRun: (nodeIds?: string[]) => void;
  onManageDevices: () => void;
  onConfigure: (deployment: api.Deployment) => void;
};

function releaseLabel(version: string | undefined): string | undefined {
  const release = version?.match(/^v?(\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.?\d*)?)(?:$|[+.-])/i)?.[1];
  return release ? `v${release}` : undefined;
}

export function RecipeOverview({ recipe, repository, onRun, onManageDevices, onConfigure }: RecipeOverviewProps) {
  const nodes = useNodes();
  const deployments = useDeployments();
  const packages = new Map<string, api.Recipe>();
  for (const version of repository?.versions ?? []) packages.set(version.recipe.digest, version.recipe);
  if (repository?.current_recipe) packages.set(repository.current_recipe.digest, repository.current_recipe);
  packages.set(recipe.digest, recipe);
  const relatedDigests = new Set(packages.keys());
  for (const device of repository?.installed_devices ?? []) {
    for (const digest of device.installed_digests) relatedDigests.add(digest);
  }
  const versionLabels = new Map<string, string>();
  const releases = new Map<string, number>();
  for (const saved of packages.values()) {
    const release = releaseLabel(saved.version);
    if (release) releases.set(release, (releases.get(release) ?? 0) + 1);
  }
  for (const digest of relatedDigests) {
    const release = releaseLabel(packages.get(digest)?.version);
    const number = versionLabels.size + 1;
    versionLabels.set(digest, release && releases.get(release) === 1 ? release : `${release ? `${release} · ` : ""}package ${number}`);
  }
  const versionLabel = (digest: string) => versionLabels.get(digest) ?? "Version not recorded";
  const nodeById = new Map((nodes.data ?? []).map((node) => [node.id, node]));
  const currentDeployments = (deployments.data ?? []).filter((deployment) => relatedDigests.has(deployment.recipe_digest) && isCurrentDeployment(deployment));
  const devices = repository?.installed_devices.filter((device) => device.installed_digests.length > 0) ?? [];
  const latestSavedDigest = (digests: string[]) => {
    let latest = digests[0];
    let savedAt = -Infinity;
    for (const version of repository?.versions ?? []) {
      const timestamp = Date.parse(version.installed_at);
      if (digests.includes(version.recipe.digest) && timestamp > savedAt) {
        latest = version.recipe.digest;
        savedAt = timestamp;
      }
    }
    return latest;
  };
  const deviceStatus = (nodeId: string) => {
    if (nodes.isError || nodes.isPending) return "unknown";
    const node = nodeById.get(nodeId);
    if (!node) return "unknown";
    return node.status === "online" && !isLiveNode(node) ? "unknown" : node.status;
  };

  return <section className="space-y-4" aria-label="Recipe overview">
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="space-y-1">
        <h2 className="font-display text-xl font-semibold">On your devices</h2>
        <p className="text-sm text-muted">Selected: {versionLabel(recipe.digest)} · Saved in library</p>
      </div>
      <Button size="sm" variant="outline" onClick={onManageDevices}>Manage device files & versions</Button>
    </div>
    {nodes.isError ? <RecipeError error={nodes.error} retry={() => void nodes.refetch()} /> : null}
    {deployments.isError ? <RecipeError error={deployments.error} retry={() => void deployments.refetch()} /> : null}
    {nodes.isPending ? <p className="text-sm text-muted" role="status">Loading device status…</p> : null}
    <div className="grid gap-4 lg:grid-cols-2">
      <section className="control space-y-3 p-4" aria-label="Saved packages">
        <div className="flex items-baseline justify-between gap-2"><h3 className="font-semibold">Saved packages</h3><span className="text-xs text-muted">{packages.size} in library</span></div>
        <p className="text-xs text-muted">Saved recipe packages, not running models.</p>
        {devices.length ? <ul className="divide-y divide-rule">{devices.map((device) => <li key={device.node_id} className="space-y-2 py-3 first:pt-0 last:pb-0">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Link className="text-sm font-medium underline" to={`/fleet/nodes/${encodeURIComponent(device.node_id)}`}>{nodeById.get(device.node_id)?.display_name || device.node_name || "Device name unavailable"}</Link>
            <StatusDot state={deviceStatus(device.node_id)} />
          </div>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-xs text-muted">{device.installed_digests.includes(recipe.digest) ? `${versionLabel(recipe.digest)} · selected` : `${versionLabel(latestSavedDigest(device.installed_digests))} · latest saved`}{device.installed_digests.length > 1 ? ` · ${device.installed_digests.length - 1} other saved` : ""}</p>
            <Button size="sm" variant="ghost" onClick={() => onRun([device.node_id])}>Run here</Button>
          </div>
        </li>)}</ul> : <p className="text-sm text-muted">{repository ? "No saved device packages reported for this repository." : "Device package locations are not reported for standalone recipes."}</p>}
      </section>
      <section className="control space-y-3 p-4" aria-label="Deployments">
        <h3 className="font-semibold">Deployments</h3>
        {deployments.isPending ? <p className="text-sm text-muted" role="status">Loading deployments…</p> : currentDeployments.length ? <ul className="divide-y divide-rule">{currentDeployments.map((deployment) => {
          const placements = deployment.placements ?? [];
          const deviceIds = [...new Set(placements.map((placement) => placement.node_id))];
          const live = !nodes.isError && !nodes.isPending && deviceIds.length > 0 && deviceIds.every((id) => isLiveNode(nodeById.get(id)));
          const unconfirmed = deployment.observed_state === "healthy" && (!live || deployments.isError);
          return <li key={deployment.id} className="space-y-2 py-3 first:pt-0 last:pb-0">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Link className="text-sm font-medium underline" to={`/serving/deployments/${encodeURIComponent(deployment.id)}`}>{versionLabel(deployment.recipe_digest)}</Link>
              <StatusDot state={unconfirmed ? "unknown" : deployment.observed_state} label={unconfirmed ? "Status unconfirmed" : undefined} />
            </div>
            <div className="flex flex-wrap gap-x-3 gap-y-1 text-xs">{deviceIds.length ? deviceIds.map((nodeId) => <span className="inline-flex items-center gap-2" key={nodeId}><Link className="underline" to={`/fleet/nodes/${encodeURIComponent(nodeId)}`}>{nodeById.get(nodeId)?.display_name || placements.find((placement) => placement.node_id === nodeId)?.node_name || "Device name unavailable"}</Link><StatusDot state={deviceStatus(nodeId)} /></span>) : <span className="text-muted">No device placement reported</span>}</div>
            <p className="text-xs text-muted">Requested: {deployment.desired_state}{unconfirmed ? " · Last reported healthy; live status unavailable" : ""}</p>
            <div className="flex flex-wrap items-center gap-3"><Button size="sm" variant="outline" onClick={() => onConfigure(deployment)}>Configure</Button><Link className="text-xs underline" to={`/serving/deployments/${encodeURIComponent(deployment.id)}`}>Stop controls & logs</Link></div>
          </li>;
        })}</ul> : !deployments.isError ? <p className="text-sm text-muted">{repository ? "No active deployments for any saved version of this repository." : "No active deployments for this package."}</p> : <p className="text-sm text-muted">Deployment status is unavailable.</p>}
      </section>
    </div>
  </section>;
}
