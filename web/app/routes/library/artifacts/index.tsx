import { useState } from "react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useArtifacts, useNodes } from "~/lib/queries";
import type { ArtifactKind } from "~/lib/api";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { bytes, shortId } from "~/lib/format";

const KINDS: (ArtifactKind | "all")[] = [
  "all",
  "model",
  "dataset",
  "adapter",
  "checkpoint",
  "recipe",
  "image",
  "result",
  "file",
];

export default function ArtifactsRoute() {
  const [kind, setKind] = useState<ArtifactKind | "all">("all");
  const { data, isPending, isError, error, refetch } = useArtifacts(
    kind === "all" ? {} : { kind },
  );
  const { data: nodes } = useNodes();

  const nodeName = (nid: string) => nodes?.find((n) => n.id === nid)?.display_name ?? shortId(nid);
  const size = (a: { metadata?: Record<string, unknown> }): string => {
    const m = a.metadata;
    if (!m) return "—";
    for (const key of ["size", "size_bytes", "bytes"]) {
      const v = m[key];
      if (typeof v === "number") return bytes(v);
    }
    return "—";
  };

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">artifacts</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} tracked</span>
          <Select value={kind} onValueChange={(v) => setKind(v as ArtifactKind | "all")}>
            <SelectTrigger className="ml-auto h-7 w-36 font-mono text-xs" aria-label="Filter by kind">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {KINDS.map((k) => (
                <SelectItem key={k} value={k}>
                  {k}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading artifacts…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load artifacts"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No artifacts"
            hint="Models, datasets, and images registered by agents or deployment plans appear here with their per-node placements."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Identity</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>Revision</TableHead>
                  <TableHead>Size</TableHead>
                  <TableHead>Validation</TableHead>
                  <TableHead>Placements</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>
                      <span className="block max-w-72 truncate font-mono text-xs" title={a.identity}>
                        {a.identity}
                      </span>
                      <span className="font-mono text-[10px] text-faint">{shortId(a.id)}</span>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted">{a.kind}</TableCell>
                    <TableCell className="font-mono text-[11px] text-faint">
                      {a.revision ? shortId(a.revision) : "—"}
                    </TableCell>
                    <TableCell className="font-mono text-xs tnum">{size(a)}</TableCell>
                    <TableCell>
                      <StatusDot state={a.validation_state} />
                    </TableCell>
                    <TableCell>
                      <div className="flex max-w-72 flex-wrap gap-1">
                        {(a.placements ?? []).length === 0 ? (
                          <span className="font-mono text-[11px] text-faint">none</span>
                        ) : (
                          (a.placements ?? []).map((p) => (
                            <span
                              key={`${p.node_id}-${p.path}`}
                              title={`${p.path} (${p.state})`}
                              className="rounded border border-hairline bg-raised/60 px-1.5 py-0.5 font-mono text-[10px] text-muted"
                            >
                              {nodeName(p.node_id)}
                              {p.state === "invalid" ? "!" : ""}
                            </span>
                          ))
                        )}
                      </div>
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
