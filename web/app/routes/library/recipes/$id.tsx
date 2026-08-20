import { useParams } from "react-router";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useRecipe, useSetRecipeTrust, useDeleteRecipe } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { HighlightedPre } from "~/components/json-viewer";
import { shortDigest, toYaml, wallClock } from "~/lib/format";

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="lmw-panel">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
      </header>
      {children}
    </section>
  );
}

/** Recipe detail: metadata panel, trust actions, and the manifest document. */
export default function RecipeDetailRoute() {
  const { id } = useParams();
  const { data: recipe, isPending, isError, error, refetch } = useRecipe(id);
  const setTrust = useSetRecipeTrust();
  const remove = useDeleteRecipe();

  if (isPending) {
    return <p className="py-10 text-center font-mono text-xs text-faint">loading recipe…</p>;
  }
  if (isError) {
    return (
      <EmptyState
        title="Cannot load recipe"
        detail={error instanceof Error ? error.message : undefined}
        onRetry={() => void refetch()}
      />
    );
  }
  if (!recipe) return null;

  const manifestText = recipe.manifest ? toYaml(recipe.manifest) : null;

  const trust = (trust_state: "local" | "untrusted") =>
    setTrust
      .mutateAsync({ digest: recipe.digest, trust_state, permission_diff_accepted: true })
      .then((r) => toast.success(`Trust set to ${r.trust_state}`))
      .catch((e) => toast.error(e instanceof Error ? e.message : "trust update failed"));

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <div className="flex items-center gap-2">
            <h1 className="lmw-label">recipe · {recipe.name}</h1>
            <span className="font-mono text-xs text-muted">{recipe.version}</span>
            <span className="font-mono text-[10px] text-faint">{shortDigest(recipe.digest)}</span>
          </div>
          <div className="ml-auto flex items-center gap-2">
            {recipe.trust_state !== "local" ? (
              <ConfirmDialog
                title="Mark recipe local"
                description={`Accept the permission diff for ${recipe.name} and mark it operator-trusted (local)? Untrusted recipes cannot be launched.`}
                confirmLabel="accept diff"
                onConfirm={() => void trust("local")}
              >
                mark local
              </ConfirmDialog>
            ) : null}
            {recipe.trust_state !== "untrusted" ? (
              <Button
                size="sm"
                variant="outline"
                disabled={setTrust.isPending}
                onClick={() => void trust("untrusted")}
              >
                mark untrusted
              </Button>
            ) : null}
            <ConfirmDialog
              title="Delete recipe"
              description={`Remove ${recipe.name}@${recipe.version}? Running deployments are not affected until restarted.`}
              confirmLabel="delete"
              tone="destructive"
              onConfirm={async () => {
                try {
                  await remove.mutateAsync(recipe.digest);
                  toast.success("Recipe deleted");
                } catch (e) {
                  toast.error(e instanceof Error ? e.message : "delete failed");
                  throw e;
                }
              }}
            >
              delete
            </ConfirmDialog>
          </div>
        </header>

        <div className="grid gap-4 p-3 md:grid-cols-2">
          <div className="grid gap-1 font-mono text-xs">
            <p className="lmw-label mb-1">provenance</p>
            <p>
              <span className="text-muted">name</span>{" "}
              <span className="text-foreground">
                {recipe.name} {recipe.version}
              </span>
            </p>
            {recipe.source ? (
              <p>
                <span className="text-muted">source</span>{" "}
                <span className="text-foreground">
                  {recipe.source.type}
                  {recipe.source.remote ? ` ${recipe.source.remote}` : ""}
                  {recipe.source.revision ? ` @${shortDigest(recipe.source.revision)}` : ""}
                  {recipe.source.reference ? ` ${recipe.source.reference}` : ""}
                  {recipe.source.path ? ` ${recipe.source.path}` : ""}
                </span>
              </p>
            ) : null}
            {recipe.license ? (
              <p>
                <span className="text-muted">license</span> <span>{recipe.license}</span>
              </p>
            ) : null}
            <p>
              <span className="text-muted">installed</span>{" "}
              <span>{wallClock(recipe.installed_at)}</span>
            </p>
          </div>

          <div className="grid gap-1 font-mono text-xs">
            <p className="lmw-label mb-1">permissions &amp; compatibility</p>
            <p>
              <span className="text-muted">trust</span>{" "}
              <span className={recipe.trust_state === "untrusted" ? "text-fault" : recipe.trust_state === "local" ? "text-warn" : "text-ok"}>
                {recipe.trust_state}
              </span>
            </p>
            <p>
              <span className="text-muted">profiles</span>{" "}
              <span>{recipe.profile_count ?? 0}</span> ·{" "}
              <span className="text-muted">variants</span>{" "}
              <span>{recipe.variant_count ?? 0}</span> ·{" "}
              <span className="text-muted">artifacts</span>{" "}
              <span>{recipe.artifact_count ?? 0}</span>
            </p>
            {recipe.permissions && recipe.permissions.length > 0 ? (
              <p>
                <span className="text-muted">permissions</span>{" "}
                <span>{recipe.permissions.join(", ")}</span>
              </p>
            ) : null}
            {recipe.high_risk && recipe.high_risk.length > 0 ? (
              <p className="text-warn">
                <span className="text-muted">high-risk</span> {recipe.high_risk.join(", ")}
              </p>
            ) : null}
            {recipe.compatibility?.node_count ? (
              <p>
                <span className="text-muted">nodes</span>{" "}
                <span>{recipe.compatibility.node_count}</span>
              </p>
            ) : null}
          </div>
        </div>
      </div>

      {recipe.updates && recipe.updates.length > 0 ? (
        <Section title="available updates">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Version</TableHead>
                  <TableHead>Digest</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {recipe.updates.map((u) => (
                  <TableRow key={u.digest ?? u.version}>
                    <TableCell className="font-mono text-xs">{u.version ?? "unknown"}</TableCell>
                    <TableCell className="font-mono text-[11px] text-muted">
                      {shortDigest(u.digest)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </Section>
      ) : null}

      <Section title="manifest (localmodelworks/v1alpha1)">
        {manifestText ? (
          <div className="p-3">
            <HighlightedPre text={manifestText} language="yaml" maxLines={400} />
          </div>
        ) : (
          <p className="px-3 py-6 text-center font-mono text-xs text-faint">no manifest available</p>
        )}
      </Section>
    </div>
  );
}
