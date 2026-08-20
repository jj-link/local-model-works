import { useState } from "react";
import { Link } from "react-router";
import { Download } from "lucide-react";
import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useRecipes } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { shortDigest, wallClock } from "~/lib/format";

const SOURCE_ICON: Record<string, string> = {
  catalog: "◈",
  oci: "◉",
  git: "⎇",
  local: "▣",
};

export default function RecipesRoute() {
  const { data, isPending, isError, error, refetch } = useRecipes();
  const [importOpen, setImportOpen] = useState(false);

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">recipes</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} installed</span>
          <Button size="sm" className="ml-auto" onClick={() => setImportOpen(true)}>
            <Download aria-hidden /> import recipe
          </Button>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading recipes…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load recipes"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No recipes installed"
            hint="Import from a signed catalog, an OCI reference, a pinned Git commit, or a local path."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Recipe</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Trust</TableHead>
                  <TableHead>Compatibility</TableHead>
                  <TableHead>Installed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((r) => (
                  <TableRow key={r.digest}>
                    <TableCell>
                      <Link
                        to={`/library/recipes/${r.digest}`}
                        className="control font-medium hover:text-foreground"
                      >
                        {r.name}
                      </Link>
                      {r.version_count && r.version_count > 1 ? (
                        <span className="ml-2 font-mono text-[10px] text-faint">
                          +{r.version_count - 1} version{r.version_count - 1 === 1 ? "" : "s"}
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted">{r.version}</TableCell>
                    <TableCell>
                      {r.source ? (
                        <span className="inline-flex items-center gap-1.5 font-mono text-xs text-muted">
                          <span aria-hidden>{SOURCE_ICON[r.source.type] ?? "·"}</span>
                          {r.source.type}
                          {r.source.remote ? (
                            <span className="text-faint">
                              {r.source.remote.length > 34 ? shortDigest(r.source.remote) : r.source.remote}
                            </span>
                          ) : null}
                        </span>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          r.trust_state === "verified"
                            ? "default"
                            : r.trust_state === "local"
                              ? "secondary"
                              : "outline"
                        }
                        className={
                          r.trust_state === "untrusted"
                            ? "border-fault/50 text-fault"
                            : r.trust_state === "verified"
                              ? "text-ok"
                              : "text-warn"
                        }
                      >
                        {r.trust_state}
                      </Badge>
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-muted">
                      {r.compatibility?.node_count
                        ? `${r.compatibility.node_count} node${r.compatibility.node_count === 1 ? "" : "s"}`
                        : "1 node"}
                    </TableCell>
                    <TableCell>
                      <span className="font-mono text-[11px] text-faint" title={r.digest}>
                        {shortDigest(r.digest)}
                      </span>{" "}
                      <span className="font-mono text-[11px] text-muted">
                        {wallClock(r.installed_at)}
                      </span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      <ImportRecipeDialog open={importOpen} onOpenChange={setImportOpen} />
    </div>
  );
}
