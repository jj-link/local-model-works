import { Link } from "react-router";
import { Boxes } from "lucide-react";
import { Badge } from "~/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useModules } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";

/** Module manifest: what the control plane exposes and how it navigates. */
export default function ModulesRoute() {
  const { data, isPending, isError, error, refetch } = useModules();

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">modules</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} registered</span>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading modules…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load modules"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState className="m-3" title="No modules" icon={<Boxes aria-hidden />} />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Module</TableHead>
                  <TableHead>Title</TableHead>
                  <TableHead>Route</TableHead>
                  <TableHead>Job kinds</TableHead>
                  <TableHead>Artifact kinds</TableHead>
                  <TableHead>Capabilities</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((m) => (
                  <TableRow key={m.id}>
                    <TableCell>
                      <span className="font-mono text-xs">{m.id}</span>
                    </TableCell>
                    <TableCell className="text-xs">{m.title}</TableCell>
                    <TableCell>
                      {m.route ? (
                        <Link to={m.route} className="control font-mono text-xs text-accent hover:underline">
                          {m.route}
                        </Link>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <div className="flex max-w-56 flex-wrap gap-1">
                        {(m.jobKinds ?? []).length === 0 ? (
                          <span className="text-faint">—</span>
                        ) : (
                          (m.jobKinds ?? []).map((k) => (
                            <Badge key={k} variant="secondary" className="font-mono text-[10px]">
                              {k}
                            </Badge>
                          ))
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex max-w-56 flex-wrap gap-1">
                        {(m.artifactKinds ?? []).length === 0 ? (
                          <span className="text-faint">—</span>
                        ) : (
                          (m.artifactKinds ?? []).map((k) => (
                            <Badge key={k} variant="outline" className="font-mono text-[10px]">
                              {k}
                            </Badge>
                          ))
                        )}
                      </div>
                    </TableCell>
                    <TableCell className="max-w-64 font-mono text-[10px] text-muted">
                      {(m.capabilities ?? []).join(", ") || "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
    </div>
  );
}
