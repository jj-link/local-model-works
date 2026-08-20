import { useState } from "react";
import { Link } from "react-router";
import { Plus } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useFabrics, useNodes } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { CreateFabricDialog } from "~/components/dialogs/create-fabric-dialog";

export default function FabricsRoute() {
  const { data, isPending, isError, error, refetch } = useFabrics();
  const { data: nodes } = useNodes();
  const [createOpen, setCreateOpen] = useState(false);

  const nodeName = (id: string) => nodes?.find((n) => n.id === id)?.display_name ?? id.slice(0, 8);

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">fabrics</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} configured</span>
          <Button size="sm" className="ml-auto" onClick={() => setCreateOpen(true)}>
            <Plus aria-hidden /> new fabric
          </Button>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading fabrics…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load fabrics"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No fabrics configured"
            hint="A fabric groups ordered node members on a shared transport (RoCE or TCP) for multi-rank serving and peer transfers."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Transport</TableHead>
                  <TableHead>Members</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Version</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((f) => (
                  <TableRow key={f.id}>
                    <TableCell>
                      <Link to={`/fleet/fabrics/${f.id}`} className="control font-medium hover:text-foreground">
                        {f.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted">{f.transport}</TableCell>
                    <TableCell>
                      <span className="font-mono text-xs">
                        {f.members.length} · {f.members.map(nodeName).join(" → ")}
                      </span>
                    </TableCell>
                    <TableCell>
                      <StatusDot state={f.state} />
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-faint">
                      {f.version ?? "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      <CreateFabricDialog open={createOpen} onOpenChange={setCreateOpen} />
    </div>
  );
}
