import { useState } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { Pencil, RefreshCcw } from "lucide-react";
import { Button } from "~/components/ui/button";
import { useTailPathParam } from "~/lib/path-param";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useFabric, useNodes, useUpdateFabric, useDeleteFabric } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { DiagnosticsList } from "~/components/diagnostics-list";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { CreateFabricDialog } from "~/components/dialogs/create-fabric-dialog";
import { shortId } from "~/lib/format";

/**
 * Fabric detail: member table with per-member diagnostics, re-validate
 * (PUT re-runs validation server-side) and delete.
 */
export default function FabricDetailRoute() {
  const id = useTailPathParam();
  const { data: fabric, isPending, isError, error, refetch } = useFabric(id);
  const { data: nodes } = useNodes();
  const update = useUpdateFabric();
  const remove = useDeleteFabric();
  const [editOpen, setEditOpen] = useState(false);

  const nodeName = (nid: string) => nodes?.find((n) => n.id === nid);

  const revalidate = async () => {
    if (!fabric?.version) return;
    try {
      const f = await update.mutateAsync({
        id: fabric.id,
        name: fabric.name,
        transport: fabric.transport,
        members: fabric.members,
        bindings: fabric.bindings,
        ifMatch: fabric.version,
      });
      toast.success("Fabric re-validated", { description: `${f.name} · ${f.state}` });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "re-validation failed");
    }
  };

  if (isPending) {
    return <p className="py-10 text-center font-mono text-xs text-faint">loading fabric…</p>;
  }
  if (isError) {
    return (
      <EmptyState
        title="Cannot load fabric"
        detail={error instanceof Error ? error.message : undefined}
        onRetry={() => void refetch()}
      />
    );
  }
  if (!fabric) return null;

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <div className="flex items-center gap-2">
            <h1 className="lmw-label">fabric · {fabric.name}</h1>
            <StatusDot state={fabric.state} />
          </div>
          <span className="font-mono text-[11px] text-faint">
            {fabric.transport} · {fabric.members.length} members · node-specific bindings
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button size="sm" variant="outline" onClick={() => setEditOpen(true)}>
              <Pencil aria-hidden /> edit wiring
            </Button>
            <Button size="sm" variant="outline" onClick={() => void revalidate()} disabled={update.isPending || !fabric.version}>
              <RefreshCcw aria-hidden /> {update.isPending ? "validating…" : "re-validate"}
            </Button>
            <ConfirmDialog
              title="Delete fabric"
              description={`Remove ${fabric.name} and its ${fabric.members.length} member bindings? Deployments using it keep running until stopped.`}
              confirmLabel="delete"
              tone="destructive"
              onConfirm={async () => {
                try {
                  await remove.mutateAsync({ id: fabric.id, ifMatch: fabric.version ?? "" });
                  toast.success("Fabric deleted");
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

        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Role</TableHead>
                <TableHead>Node</TableHead>
                <TableHead>Interface / address</TableHead>
                <TableHead>RDMA / GID</TableHead>
                <TableHead>State</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {fabric.members.map((nid, i) => {
                const n = nodeName(nid);
                const binding = fabric.bindings.find((candidate) => candidate.node_id === nid);
                return (
                  <TableRow key={`${nid}-${i}`}>
                    <TableCell className="font-mono text-xs">{i === 0 ? "Head · API" : `Worker ${i}`}</TableCell>
                    <TableCell>
                      <Link to={`/fleet/nodes/${nid}`} className="control hover:text-foreground">
                        {n?.display_name ?? shortId(nid)}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-[11px]">
                      <span className="block">{binding?.interface_name || "not configured"}</span>
                      <span className="text-muted">{binding?.address || "address missing"}</span>
                    </TableCell>
                    <TableCell className="font-mono text-[11px]">
                      <span className="block">{binding?.rdma_device || (fabric.transport === "roce" ? "not configured" : "not required")}</span>
                      <span className="text-muted">GID {binding?.gid_index ?? "—"}</span>
                    </TableCell>
                    <TableCell><StatusDot state={n?.status ?? "unknown"} /></TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>

        {fabric.diagnostics && fabric.diagnostics.length > 0 ? (
          <div className="border-t border-hairline p-3">
            <p className="lmw-label mb-2">diagnostics</p>
            <DiagnosticsList diagnostics={fabric.diagnostics} />
          </div>
        ) : null}
      </div>
      <CreateFabricDialog open={editOpen} onOpenChange={setEditOpen} existing={fabric} />
    </div>
  );
}
