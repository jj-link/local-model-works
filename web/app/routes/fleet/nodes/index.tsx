import { Link } from "react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "~/components/ui/table";
import { useNodes } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { relativeTime } from "~/lib/format";
import { useSession } from "~/lib/api/session";

export default function NodesRoute() {
  const { data, isPending, isError, error, refetch } = useNodes();
  const session = useSession();

  const pendingNodes = (data ?? []).filter((n) => n.status === "pending");

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">nodes</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} enrolled</span>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading nodes…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load nodes"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No nodes enrolled"
            hint="Create an enrollment token from the rail (Enroll node) and run the printed lmw-agent install command on each host."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table aria-label="Nodes">
              <TableHeader>
                <TableRow>
                  <TableHead>Node</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Accelerators</TableHead>
                  <TableHead>Host</TableHead>
                  <TableHead>Agent</TableHead>
                  <TableHead>Last seen</TableHead>
                  <TableHead aria-label="Actions" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((n) => {
                  const accels = n.inventory?.accelerators ?? [];
                  return (
                    <TableRow key={n.id}>
                      <TableCell>
                        <Link
                          to={`/fleet/nodes/${n.id}`}
                          className="font-mono text-xs font-medium text-foreground hover:text-primary"
                        >
                          {n.display_name}
                        </Link>
                        {n.inventory?.hostname ? (
                          <span className="ml-2 font-mono text-[10px] text-faint">{n.inventory.hostname}</span>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <StatusDot state={n.status} pulse={n.status === "online"} />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {accels.length > 0
                          ? `${accels.length}× ${accels[0].vendor ?? "?"} ${accels[0].name ?? ""}`
                          : <span className="text-faint">none</span>}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {n.inventory?.hostname ?? <span className="text-faint">—</span>}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {n.agent_version ? (
                          <>
                            <span className="text-faint">v</span>
                            {n.agent_version}
                          </>
                        ) : (
                          <span className="text-faint">—</span>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {relativeTime(n.last_heartbeat)}
                      </TableCell>
                      <TableCell>
                        {n.status === "pending" && session ? (
                          <Link to={`/fleet/nodes/${n.id}`} className="control rounded border border-warn/60 bg-warn/10 px-2 py-0.5 font-mono text-[11px] text-warn hover:bg-warn/20">
                            review →
                          </Link>
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

      {pendingNodes.length > 0 ? (
        <p className="font-mono text-[11px] text-warn">
          {pendingNodes.length} node{pendingNodes.length === 1 ? "" : "s"} awaiting approval — review their
          reported inventory before they join the schedulable fleet.
        </p>
      ) : null}
    </div>
  );
}
