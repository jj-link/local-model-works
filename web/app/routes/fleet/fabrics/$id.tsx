import { Link, useParams } from "react-router";
import { toast } from "sonner";
import { RefreshCcw } from "lucide-react";
import { Button } from "~/components/ui/button";
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
import { shortId } from "~/lib/format";

/**
 * Fabric detail: member table with per-member diagnostics, re-validate
 * (PUT re-runs validation server-side) and delete.
 */
export default function FabricDetailRoute() {
  const { id } = useParams();
  const { data: fabric, isPending, isError, error, refetch } = useFabric(id);
  const { data: nodes } = useNodes();
  const update = useUpdateFabric();
  const remove = useDeleteFabric();

  const nodeName = (nid: string) => nodes?.find((n) => n.id === nid);

  const revalidate = async () => {
    if (!fabric?.version) return;
    try {
      const f = await update.mutateAsync({
        id: fabric.id,
        name: fabric.name,
        transport: fabric.transport,
        members: fabric.members,
        ...(fabric.interface_name ? { interface_name: fabric.interface_name } : {}),
        ...(fabric.address ? { address: fabric.address } : {}),
        ...(fabric.rdma_device ? { rdma_device: fabric.rdma_device } : {}),
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
            {fabric.transport}
            {fabric.interface_name ? ` · ${fabric.interface_name}` : ""}
            {fabric.rdma_device ? ` · ${fabric.rdma_device}` : ""}
            {fabric.address ? ` · ${fabric.address}` : ""}
          </span>
          <div className="ml-auto flex items-center gap-2">
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
                <TableHead>Rank</TableHead>
                <TableHead>Node</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Heartbeat</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {fabric.members.map((nid, i) => {
                const n = nodeName(nid);
                return (
                  <TableRow key={`${nid}-${i}`}>
                    <TableCell className="font-mono text-xs">{i}</TableCell>
                    <TableCell>
                      <Link to={`/fleet/nodes/${nid}`} className="control hover:text-foreground">
                        {n?.display_name ?? shortId(nid)}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <StatusDot state={n?.status ?? "unknown"} />
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-muted">
                      {n ? new Date(n.last_heartbeat ?? 0).toLocaleString() : "unknown"}
                    </TableCell>
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
    </div>
  );
}
