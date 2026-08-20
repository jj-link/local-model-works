import { Link } from "react-router";
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
import { useTransfers, useCancelTransfer, useNodes, useArtifacts } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { bytes, shortId, wallClock } from "~/lib/format";

const ACTIVE = new Set(["pending", "transferring", "validating"]);

export default function TransfersRoute() {
  const { data, isPending, isError, error, refetch } = useTransfers();
  const cancel = useCancelTransfer();
  const { data: nodes } = useNodes();
  const { data: artifacts } = useArtifacts();

  const nodeName = (nid: string) => nodes?.find((n) => n.id === nid)?.display_name ?? shortId(nid);
  const artifactIdentity = (aid: string) => artifacts?.find((a) => a.id === aid)?.identity;

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">transfers</h1>
          <span className="font-mono text-[11px] text-faint">
            {(data ?? []).filter((t) => ACTIVE.has(t.state)).length} active
          </span>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading transfers…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load transfers"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No transfers"
            hint="Peer transfers previewed in deployment plans (fabric-aware, resumable) appear here while in flight."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>Artifact</TableHead>
                  <TableHead>Path</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Progress</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((t) => {
                  const total = t.bytes_total ?? 0;
                  const done = t.bytes_done ?? 0;
                  const pct = total > 0 ? Math.min(100, (done / total) * 100) : 0;
                  const identity = artifactIdentity(t.artifact_id);
                  return (
                    <TableRow key={t.id}>
                      <TableCell className="font-mono text-[11px] text-faint">{shortId(t.id)}</TableCell>
                      <TableCell>
                        {identity ? (
                          <span className="block max-w-56 truncate font-mono text-xs" title={identity}>
                            {identity}
                          </span>
                        ) : (
                          <span className="font-mono text-[11px] text-faint">{shortId(t.artifact_id)}</span>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        <Link to={`/fleet/nodes/${t.source_node}`} className="control hover:text-foreground">
                          {nodeName(t.source_node)}
                        </Link>
                        <span aria-hidden> → </span>
                        <Link to={`/fleet/nodes/${t.dest_node}`} className="control hover:text-foreground">
                          {nodeName(t.dest_node)}
                        </Link>
                      </TableCell>
                      <TableCell>
                        <StatusDot state={t.state} pulse={t.state === "transferring"} />
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <progress
                            className="h-1 w-24 accent-primary"
                            value={t.state === "complete" ? 100 : pct}
                            max={100}
                            aria-label={`Transfer progress: ${Math.round(pct)}%`}
                          />
                          <span className="font-mono text-[10px] tnum text-muted">
                            {total > 0 ? `${bytes(done)} / ${bytes(total)}` : "—"}
                          </span>
                        </div>
                      </TableCell>
                      <TableCell className="font-mono text-[11px] text-faint">
                        {t.created_at ? wallClock(t.created_at) : "—"}
                      </TableCell>
                      <TableCell>
                        {ACTIVE.has(t.state) ? (
                          <Button
                            size="sm"
                            variant="outline"
                            aria-label={`Cancel transfer ${shortId(t.id)}`}
                            disabled={cancel.isPending}
                            onClick={() =>
                              cancel
                                .mutateAsync(t.id)
                                .then(() => toast.success("Transfer cancelled"))
                                .catch((e) =>
                                  toast.error(e instanceof Error ? e.message : "cancel failed"),
                                )
                            }
                          >
                            cancel
                          </Button>
                        ) : t.diagnostic ? (
                          <span className="font-mono text-[10px] text-fault" title={t.diagnostic.message}>
                            {t.diagnostic.code}
                          </span>
                        ) : null}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
    </div>
  );
}
