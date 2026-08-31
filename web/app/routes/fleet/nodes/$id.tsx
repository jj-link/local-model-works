import { toast } from "sonner";
import { Cpu, HardDrive, RefreshCcw, Settings2 } from "lucide-react";
import { useTailPathParam } from "~/lib/path-param";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { Button } from "~/components/ui/button";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { JsonTree } from "~/components/json-viewer";
import { bytes, relativeTime, shortId, stateInfo, TONE_TEXT } from "~/lib/format";
import {
  useApproveNode,
  useNode,
  useRotateCertificate,
  useTransfers,
} from "~/lib/queries";
import { eventsForNode, useEventFeed } from "~/lib/events";
import { useSession } from "~/lib/api/session";
import { cn } from "~/lib/utils";
import { NodeMonitor } from "~/components/fleet/node-monitor";

function Section({
  title,
  children,
  extra,
}: {
  title: string;
  children: React.ReactNode;
  extra?: React.ReactNode;
}) {
  return (
    <section className="lmw-panel">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
        {extra}
      </header>
      {children}
    </section>
  );
}

export default function NodeDetailRoute() {
  const id = useTailPathParam();
  const nodeQ = useNode(id);
  const transfersQ = useTransfers();
  const events = useEventFeed();
  const approve = useApproveNode();
  const rotate = useRotateCertificate();
  const session = useSession();
  const node = nodeQ.data;

  const nodeTransfers = (transfersQ.data ?? []).filter(
    (t) => t.source_node === id || t.dest_node === id,
  );
  const nodeEvents = eventsForNode(events, id ?? "").slice(0, 30);

  const onApprove = async () => {
    if (!node) return;
    try {
      const updated = await approve.mutateAsync(node.id);
      toast.success("Node approved", { description: updated.display_name });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "approval failed");
    }
  };

  const onRotate = async () => {
    if (!node) return;
    try {
      const r = await rotate.mutateAsync(node.id);
      toast.success("Certificate rotated", {
        description: `new cert expires ${new Date(r.expires_at).toISOString().slice(0, 10)}`,
      });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "rotation failed");
    }
  };

  if (nodeQ.isPending) {
    return <p className="font-mono text-xs text-faint">loading node…</p>;
  }
  if (nodeQ.isError) {
    return (
      <EmptyState
        title="Cannot load node"
        detail={nodeQ.error instanceof Error ? nodeQ.error.message : undefined}
        onRetry={() => void nodeQ.refetch()}
      />
    );
  }
  if (!node) return null;

  const inv = node.inventory;
  const accels = inv?.accelerators ?? [];
  const memTotal = accels.reduce((a, g) => a + (g.memory_bytes ?? 0), 0);
  const rdma = inv?.rdma_devices ?? [];
  const ifaces = inv?.interfaces ?? [];
  const cacheRoots = inv?.cache_roots ?? [];

  return (
    <div className="grid gap-4">
      {/* Header */}
      <div className="lmw-panel flex flex-wrap items-center gap-x-4 gap-y-2 p-4">
        <div>
          <p className="font-mono text-[10px] text-faint">node / {shortId(node.id)}</p>
          <h1 className="mt-0.5 font-display text-xl font-semibold tracking-wide text-foreground">
            {node.display_name}
          </h1>
        </div>
        <StatusDot state={node.status} pulse={node.status === "online"} />
        <div className="flex items-center gap-3 font-mono text-[11px] text-muted">
          {inv?.hostname ? <span>{inv.hostname}</span> : null}
          <span>agent v{node.agent_version ?? "?"}</span>
          <span>heartbeat {relativeTime(node.last_heartbeat)}</span>
          {node.certificate_expires_at ? (
            <span>cert → {new Date(node.certificate_expires_at).toISOString().slice(0, 10)}</span>
          ) : null}
        </div>
        <div className="ml-auto flex gap-2">
          {node.status === "pending" ? (
            <Button size="sm" onClick={() => void onApprove()} disabled={approve.isPending}>
              {approve.isPending ? "approving…" : "Approve node"}
            </Button>
          ) : null}
          {node.status === "online" ? (
            <ConfirmDialog
              title={`Rotate certificate — ${node.display_name}`}
              description="The agent re-handshakes on its next connection cycle. No workload impact expected."
              confirmLabel="Rotate"
              onConfirm={() => onRotate()}
            >
              <RefreshCcw aria-hidden /> Rotate cert
            </ConfirmDialog>
          ) : null}
        </div>
      </div>

      <NodeMonitor node={node} />
      <div className="grid gap-4 xl:grid-cols-3">
        <div className="xl:col-span-2 grid gap-4 content-start">
          <Section
            title="accelerators"
            extra={<Cpu className="h-3.5 w-3.5 text-faint" aria-hidden />}
          >
            {accels.length === 0 ? (
              <p className="px-3 py-6 text-center font-mono text-xs text-faint">
                no accelerators reported
              </p>
            ) : (
              <div className="overflow-x-auto">
                <Table aria-label="Accelerator inventory">
                  <TableHeader>
                    <TableRow>
                      <TableHead>#</TableHead>
                      <TableHead>Device</TableHead>
                      <TableHead>Vendor</TableHead>
                      <TableHead>Architecture</TableHead>
                      <TableHead>Memory</TableHead>
                      <TableHead>UUID</TableHead>
                      <TableHead>Features</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {accels.map((g) => (
                      <TableRow key={g.uuid}>
                        <TableCell className="font-mono text-xs text-muted">{g.index}</TableCell>
                        <TableCell className="font-mono text-xs text-foreground">{g.name}</TableCell>
                        <TableCell className="font-mono text-xs text-muted">{g.vendor}</TableCell>
                        <TableCell className="font-mono text-xs text-muted">
                          {g.architecture ?? <span className="text-faint">—</span>}
                        </TableCell>
                        <TableCell className="font-mono text-xs tabular-nums text-muted">
                          {bytes(g.memory_bytes)}
                        </TableCell>
                        <TableCell className="font-mono text-[10px] text-faint">{shortId(g.uuid)}</TableCell>
                        <TableCell className="font-mono text-xs text-muted">
                          {g.features?.length ? g.features.join(", ") : <span className="text-faint">—</span>}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
            {accels.length > 0 ? (
              <p className="border-t border-hairline px-3 py-2 font-mono text-[11px] text-muted">
                total accelerator memory <span className="tabular-nums text-foreground">{bytes(memTotal)}</span>
              </p>
            ) : null}
          </Section>

          <Section
            title="transfers"
            extra={<HardDrive className="h-3.5 w-3.5 text-faint" aria-hidden />}
          >
            {nodeTransfers.length === 0 ? (
              <p className="px-3 py-6 text-center font-mono text-xs text-faint">
                no transfers involving this node
              </p>
            ) : (
              <div className="overflow-x-auto">
                <Table aria-label="Transfers">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Artifact</TableHead>
                      <TableHead>Direction</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Progress</TableHead>
                      <TableHead>Created</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {nodeTransfers.slice(0, 10).map((t) => {
                      const outbound = t.source_node === id;
                      const other = outbound ? t.dest_node : t.source_node;
                      return (
                        <TableRow key={t.id}>
                          <TableCell className="font-mono text-xs text-muted">
                            {t.artifact_id.slice(0, 12)}…
                          </TableCell>
                          <TableCell className="font-mono text-xs text-muted">
                            {outbound ? "out →" : "← in"} {shortId(other)}
                          </TableCell>
                          <TableCell>
                            <StatusDot state={t.state} pulse={t.state === "transferring"} />
                          </TableCell>
                          <TableCell className="font-mono text-xs tabular-nums text-muted">
                            {t.bytes_done != null && t.bytes_total
                              ? `${((t.bytes_done / t.bytes_total) * 100).toFixed(0)}%`
                              : "—"}
                          </TableCell>
                          <TableCell className="font-mono text-xs text-muted">
                            {relativeTime(t.created_at)}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            )}
          </Section>

          <Section
            title="cache roots"
            extra={<Settings2 className="h-3.5 w-3.5 text-faint" aria-hidden />}
          >
            {cacheRoots.length === 0 ? (
              <p className="px-3 py-4 text-center font-mono text-xs text-faint">
                no model/cache roots registered
              </p>
            ) : (
              <ul className="px-3 py-2 font-mono text-xs">
                {cacheRoots.map((root) => (
                  <li key={root.path} className="border-b border-hairline/60 py-1.5 text-muted last:border-b-0">
                    <div className="flex flex-wrap items-center gap-x-3">
                      <span className="text-foreground">{root.path}</span>
                      <span>{root.backend || "filesystem"}</span>
                    </div>
                    {root.repositories?.length ? (
                      <p className="mt-1 text-[10px] text-faint">
                        {root.repositories.length} cached {root.repositories.length === 1 ? "repository" : "repositories"}
                      </p>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </Section>
        </div>

        {/* Right column */}
        <div className="grid gap-4 content-start">
          <Section title="software">
            <ul className="px-3 py-2 font-mono text-xs">
              {inv?.docker ? (
                <li className="flex justify-between border-b border-hairline/60 py-1.5 last:border-b-0">
                  <span className="text-muted">docker</span>
                  <span className={inv.docker.ok ? "text-foreground" : "text-fault"}>
                    {inv.docker.version}
                    {inv.docker.api_version ? (
                      <span className="text-faint"> · api {inv.docker.api_version}</span>
                    ) : null}
                    {inv.docker.ok ? "" : " · unavailable"}
                  </span>
                </li>
              ) : null}
              {inv?.os ? (
                <li className="flex justify-between border-b border-hairline/60 py-1.5 last:border-b-0">
                  <span className="text-muted">os</span>
                  <span className="text-foreground">{inv.os}</span>
                </li>
              ) : null}
              {inv?.arch ? (
                <li className="flex justify-between border-b border-hairline/60 py-1.5 last:border-b-0">
                  <span className="text-muted">arch</span>
                  <span className="text-foreground">{inv.arch}</span>
                </li>
              ) : null}
              {inv?.hostname ? (
                <li className="flex justify-between py-1.5">
                  <span className="text-muted">hostname</span>
                  <span className="text-foreground">{inv.hostname}</span>
                </li>
              ) : null}
            </ul>
            {rdma.length > 0 ? (
              <ul className="border-t border-hairline px-3 py-2 font-mono text-xs">
                {rdma.map((d) => (
                  <li key={d.name} className="flex justify-between py-1">
                    <span className="text-muted">rdma {d.name}</span>
                    <span className="text-right text-foreground">
                      {d.vendor ?? ""}
                      {d.network_interfaces?.length ? ` · ${d.network_interfaces.join(", ")}` : ""}
                      {(d.ports ?? []).map((port) => {
                        const populated = (port.gids ?? []).filter((gid) => /[1-9a-f]/i.test(gid.value));
                        const gidSummary = populated.length
                          ? ` · gid ${populated.map((gid) => `${gid.index}:${gid.type || "unknown"}`).join(", ")}`
                          : "";
                        return ` ${port.name ?? "?"}:${port.state ?? "?"}${port.link_rate_gbps ? `@${port.link_rate_gbps}G` : ""}${gidSummary}`;
                      }).join("")}
                    </span>
                  </li>
                ))}
              </ul>
            ) : null}
          </Section>

          <Section title="interfaces">
            {ifaces.length === 0 ? (
              <p className="px-3 py-4 text-center font-mono text-xs text-faint">
                no interfaces reported
              </p>
            ) : (
              <ul className="px-3 py-2 font-mono text-xs">
                {ifaces.map((f) => (
                  <li key={f.name} className="border-b border-hairline/60 py-1.5 last:border-b-0">
                    <span className="text-foreground">{f.name}</span>
                    <span className="ml-2 text-muted">{(f.addresses ?? []).join(" ") || "no address"}</span>
                    {f.link_mbps ? (
                      <span className="ml-auto text-faint">{f.link_mbps} Mb/s</span>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </Section>

          {node.diagnostics && node.diagnostics.length > 0 ? (
            <Section title="diagnostics">
              <ul className="px-3 py-2">
                {node.diagnostics.map((d) => (
                  <li
                    key={d.code}
                    className={cn("py-0.5 font-mono text-[11px]", TONE_TEXT[stateInfo(d.severity).tone])}
                  >
                    {d.severity} · {d.code} {d.message ? `— ${d.message}` : ""}
                  </li>
                ))}
              </ul>
            </Section>
          ) : null}

          <Section title="recent activity">
            {nodeEvents.length === 0 ? (
              <p className="px-3 py-4 text-center font-mono text-xs text-faint">
                no node events in the live ring
              </p>
            ) : (
              <ul>
                {nodeEvents.map((e) => {
                  const isErr = /error|fail|offline/i.test(e.type);
                  return (
                    <li
                      key={e.id}
                      className="flex items-baseline gap-2 border-b border-hairline/60 px-3 py-1.5 last:border-b-0"
                    >
                      <span className={cn("font-mono text-[10px] uppercase", isErr ? "text-fault" : "text-info")}>
                        {e.type.split(".").pop()}
                      </span>
                      <span className="truncate font-mono text-xs text-foreground">
                        {typeof e.payload?.message === "string" ? (e.payload.message as string) : e.type}
                      </span>
                      <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">
                        {relativeTime(new Date(e.receivedAt).toISOString())}
                      </span>
                    </li>
                  );
                })}
              </ul>
            )}
          </Section>

          <Section title="labels">
            {node.labels && Object.keys(node.labels).length > 0 ? (
              <ul className="px-3 py-2 font-mono text-xs">
                {Object.entries(node.labels).map(([k, v]) => (
                  <li key={k} className="flex justify-between border-b border-hairline/60 py-1 last:border-b-0">
                    <span className="text-muted">{k}</span>
                    <span className="text-foreground">{String(v)}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="px-3 py-4 text-center font-mono text-xs text-faint">no labels</p>
            )}
          </Section>

          <Section title="raw inventory">
            <div className="p-3">
              <JsonTree value={inv} className="max-h-80 overflow-auto" />
            </div>
          </Section>
        </div>
      </div>

      <p className="font-mono text-[10px] text-faint">
        signed in as {session?.username ?? "…"} · node id {node.id}
      </p>
    </div>
  );
}
