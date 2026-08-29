import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { useCreateFabric, useNodes, useUpdateFabric } from "~/lib/queries";
import type { Fabric, RdmaDevice } from "~/lib/api";
import { stateInfo, TONE_TEXT } from "~/lib/format";
import { cn } from "~/lib/utils";

type BindingDraft = {
  interface_name: string;
  address: string;
  rdma_device: string;
  gid_index: string;
};

const emptyBinding = (): BindingDraft => ({
  interface_name: "",
  address: "",
  rdma_device: "",
  gid_index: "3",
});

function rdmaGIDs(device?: RdmaDevice) {
  return device?.ports.flatMap((port) => port.gids ?? []) ?? [];
}

function isZeroGID(value: string) {
  return !/[1-9a-f]/i.test(value);
}

function gidForAddress(device: RdmaDevice | undefined, address: string) {
  const gids = rdmaGIDs(device);
  const octets = address.split(".").map(Number);
  if (
    octets.length === 4 &&
    octets.every((octet) => Number.isInteger(octet) && octet >= 0 && octet <= 255)
  ) {
    const group = (left: number, right: number) =>
      ((left << 8) | right).toString(16).padStart(4, "0");
    const suffix = `ffff:${group(octets[0], octets[1])}:${group(octets[2], octets[3])}`;
    const mapped = gids.filter((gid) => gid.value.toLowerCase().endsWith(suffix));
    const preferred = mapped.find((gid) => gid.type?.toLowerCase().includes("roce v2")) ??
      mapped.at(-1);
    if (preferred) return String(preferred.index);
  }
  const populated = gids.find((gid) =>
    gid.type?.toLowerCase().includes("roce v2") && !isZeroGID(gid.value)) ??
    gids.find((gid) => !isZeroGID(gid.value));
  return populated ? String(populated.index) : "3";
}

/** Create or repair an ordered fabric with node-specific transport wiring. */
export function CreateFabricDialog({
  open,
  onOpenChange,
  existing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  existing?: Fabric;
}) {
  const navigate = useNavigate();
  const { data: nodes } = useNodes();
  const create = useCreateFabric();
  const update = useUpdateFabric();
  const [name, setName] = useState("");
  const [transport, setTransport] = useState<"roce" | "tcp">("roce");
  const [members, setMembers] = useState<string[]>([]);
  const [bindings, setBindings] = useState<Record<string, BindingDraft>>({});

  useEffect(() => {
    if (!open) return;
    setName(existing?.name ?? "");
    setTransport((existing?.transport as "roce" | "tcp" | undefined) ?? "roce");
    setMembers(existing?.members ?? []);
    setBindings(Object.fromEntries((existing?.bindings ?? []).map((binding) => {
      const node = nodes?.find((candidate) => candidate.id === binding.node_id);
      const rdmaDevice = node?.inventory?.rdma_devices?.find((device) =>
        device.name === binding.rdma_device);
      return [
        binding.node_id,
        {
          interface_name: binding.interface_name,
          address: binding.address,
          rdma_device: binding.rdma_device ?? "",
          gid_index: binding.gid_index === undefined
            ? gidForAddress(rdmaDevice, binding.address)
            : String(binding.gid_index),
        },
      ];
    })));
    create.reset();
    update.reset();
  }, [open, existing]);

  const seedBinding = (nodeId: string): BindingDraft => {
    const node = nodes?.find((candidate) => candidate.id === nodeId);
    const interfaces = node?.inventory?.interfaces ?? [];
    const rdmaDevices = node?.inventory?.rdma_devices ?? [];
    const network = interfaces.find((item) =>
      item.addresses.some((address) => !address.startsWith("127.") && address !== "::1") &&
      rdmaDevices.some((device) => device.network_interfaces?.includes(item.name))) ??
      interfaces.find((item) =>
        item.addresses.some((address) => !address.startsWith("127.") && address !== "::1"));
    const address = network?.addresses.find((candidate) =>
      !candidate.startsWith("127.") && candidate !== "::1") ?? "";
    const rdmaDevice = rdmaDevices.find((device) =>
      device.network_interfaces?.includes(network?.name ?? "")) ?? rdmaDevices[0];
    return {
      interface_name: network?.name ?? "",
      address,
      rdma_device: rdmaDevice?.name ?? "",
      gid_index: gidForAddress(rdmaDevice, address),
    };
  };

  const toggleMember = (nodeId: string) => {
    setMembers((current) => current.includes(nodeId)
      ? current.filter((member) => member !== nodeId)
      : [...current, nodeId]);
    setBindings((current) => current[nodeId]
      ? current
      : { ...current, [nodeId]: seedBinding(nodeId) });
  };

  const move = (index: number, direction: -1 | 1) => {
    setMembers((current) => {
      const next = [...current];
      const destination = index + direction;
      if (destination < 0 || destination >= next.length) return current;
      [next[index], next[destination]] = [next[destination], next[index]];
      return next;
    });
  };

  const patchBinding = (nodeId: string, patch: Partial<BindingDraft>) => {
    setBindings((current) => ({
      ...current,
      [nodeId]: { ...(current[nodeId] ?? emptyBinding()), ...patch },
    }));
  };

  const submit = async () => {
    if (!name.trim() || members.length < 2) return;
    const body = {
      name: name.trim(),
      transport,
      members,
      bindings: members.map((nodeId) => {
        const binding = bindings[nodeId] ?? emptyBinding();
        return {
          node_id: nodeId,
          interface_name: binding.interface_name,
          address: binding.address,
          ...(transport === "roce" ? {
            rdma_device: binding.rdma_device,
            gid_index: Number(binding.gid_index),
          } : {}),
        };
      }),
    };
    try {
      const fabric = existing
        ? await update.mutateAsync({
            id: existing.id,
            ifMatch: existing.version ?? "",
            ...body,
          })
        : await create.mutateAsync(body);
      toast.success(existing ? "Fabric wiring updated" : "Fabric created", {
        description: `${fabric.name} · ${fabric.state}`,
      });
      onOpenChange(false);
      navigate(`/fleet/fabrics/${fabric.id}`);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "Fabric save failed");
    }
  };

  const pending = create.isPending || update.isPending;
  const ready = name.trim() && members.length >= 2 && members.every((nodeId) => {
    const binding = bindings[nodeId];
    return binding?.interface_name && binding.address &&
      (transport !== "roce" || (binding.rdma_device && binding.gid_index !== ""));
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-h-[90dvh] sm:max-w-3xl sm:overflow-auto max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-h-[100dvh] max-sm:max-w-none max-sm:overflow-auto max-sm:rounded-none">
        <DialogHeader>
          <DialogTitle className="font-display text-xl font-semibold">
            {existing ? `Repair ${existing.name}` : "Create cluster fabric"}
          </DialogTitle>
          <DialogDescription>
            Order determines workload roles. Each node keeps its own interface, address, RDMA device, and GID index.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="fabric-name">Name</Label>
              <Input
                id="fabric-name"
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="spark-p2p"
                className="font-mono text-xs"
                disabled={Boolean(existing)}
              />
            </div>
            <div className="grid gap-2">
              <Label>Transport</Label>
              <Select value={transport} onValueChange={(value) => setTransport(value as "roce" | "tcp")}>
                <SelectTrigger className="w-full" aria-label="Transport">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="roce">RoCE / RDMA</SelectItem>
                  <SelectItem value="tcp">TCP</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <section className="grid gap-2">
            <div>
              <p className="lmw-label">Cluster members</p>
              <p className="text-xs text-muted">Select nodes, then arrange the head first and workers after it.</p>
            </div>
            <div className="rounded border border-hairline" role="group" aria-label="Fabric members">
              {(nodes ?? []).map((node) => {
                const index = members.indexOf(node.id);
                const selected = index >= 0;
                return (
                  <div key={node.id} className={cn("flex items-center gap-2 border-b border-hairline px-3 py-2 last:border-b-0", selected && "bg-raised")}>
                    <button
                      type="button"
                      onClick={() => toggleMember(node.id)}
                      aria-pressed={selected}
                      aria-label={`${selected ? "Remove" : "Add"} ${node.display_name}`}
                      className={cn(
                        "control flex h-4 w-4 items-center justify-center rounded-sm border",
                        selected ? "border-primary bg-primary/20 text-primary" : "border-hairline text-transparent",
                      )}
                    >
                      ✓
                    </button>
                    <span className={cn("font-mono text-xs", selected ? "text-foreground" : "text-muted")}>{node.display_name}</span>
                    <span className={cn("text-[10px]", TONE_TEXT[stateInfo(node.status).tone])}>{stateInfo(node.status).label}</span>
                    {selected ? (
                      <span className="ml-auto flex items-center gap-1 font-mono text-[11px] text-muted">
                        {index === 0 ? "head / API" : `worker ${index}`}
                        <button type="button" onClick={() => move(index, -1)} aria-label={`Move ${node.display_name} up`} className="rounded border border-hairline px-1 control">↑</button>
                        <button type="button" onClick={() => move(index, 1)} aria-label={`Move ${node.display_name} down`} className="rounded border border-hairline px-1 control">↓</button>
                      </span>
                    ) : null}
                  </div>
                );
              })}
            </div>
          </section>

          {members.length > 0 ? (
            <section className="grid gap-3" aria-label="Member transport bindings">
              {members.map((nodeId, index) => {
                const node = nodes?.find((candidate) => candidate.id === nodeId);
                const binding = bindings[nodeId] ?? emptyBinding();
                const interfaces = node?.inventory?.interfaces ?? [];
                const addresses = interfaces.find((item) => item.name === binding.interface_name)?.addresses ?? [];
                const rdmaDevices = node?.inventory?.rdma_devices ?? [];
                const selectedRDMA = rdmaDevices.find((device) => device.name === binding.rdma_device);
                const gids = rdmaGIDs(selectedRDMA).filter((gid) => !isZeroGID(gid.value));
                return (
                  <article key={nodeId} className="overflow-hidden rounded border border-hairline">
                    <header className="flex items-center gap-3 border-b border-hairline bg-raised px-3 py-2">
                      <span className="rounded border border-primary/40 px-1.5 py-0.5 font-mono text-[10px] text-primary">R{index}</span>
                      <span className="font-display text-sm font-semibold">{node?.display_name ?? nodeId}</span>
                      <span className="ml-auto text-xs text-muted">{index === 0 ? "Head · API :8888" : "Worker · headless"}</span>
                    </header>
                    <div className={`grid gap-3 p-3 ${transport === "roce" ? "sm:grid-cols-4" : "sm:grid-cols-2"}`}>
                      <label className="grid gap-1.5">
                        <span className="lmw-label">Interface</span>
                        <select
                          aria-label={`${node?.display_name ?? nodeId} interface`}
                          value={binding.interface_name}
                          onChange={(event) => {
                            const selected = interfaces.find((item) => item.name === event.target.value);
                            const address = selected?.addresses.find((candidate) =>
                              !candidate.startsWith("127.") && candidate !== "::1") ?? "";
                            const device = rdmaDevices.find((candidate) =>
                              candidate.network_interfaces?.includes(event.target.value)) ?? rdmaDevices[0];
                            patchBinding(nodeId, {
                              interface_name: event.target.value,
                              address,
                              rdma_device: device?.name ?? "",
                              gid_index: gidForAddress(device, address),
                            });
                          }}
                          className="h-8 rounded border border-input bg-card px-2 text-xs"
                        >
                          <option value="">Select interface</option>
                          {interfaces.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
                        </select>
                      </label>
                      <label className="grid gap-1.5">
                        <span className="lmw-label">Address</span>
                        <select
                          aria-label={`${node?.display_name ?? nodeId} fabric address`}
                          value={binding.address}
                          onChange={(event) => patchBinding(nodeId, {
                            address: event.target.value,
                            gid_index: gidForAddress(selectedRDMA, event.target.value),
                          })}
                          className="h-8 rounded border border-input bg-card px-2 text-xs"
                        >
                          <option value="">Select address</option>
                          {addresses.map((address) => <option key={address} value={address}>{address}</option>)}
                        </select>
                      </label>
                      {transport === "roce" ? (
                        <>
                          <label className="grid gap-1.5">
                            <span className="lmw-label">RDMA device</span>
                            <select
                              aria-label={`${node?.display_name ?? nodeId} RDMA device`}
                              value={binding.rdma_device}
                              onChange={(event) => {
                                const device = rdmaDevices.find((candidate) => candidate.name === event.target.value);
                                patchBinding(nodeId, {
                                  rdma_device: event.target.value,
                                  gid_index: gidForAddress(device, binding.address),
                                });
                              }}
                              className="h-8 rounded border border-input bg-card px-2 text-xs"
                            >
                              <option value="">Select RDMA device</option>
                              {rdmaDevices.map((device) => <option key={device.name} value={device.name}>{device.name}</option>)}
                            </select>
                          </label>
                          <label className="grid gap-1.5">
                            <span className="lmw-label">GID index</span>
                            {gids.length > 0 ? (
                              <select
                                aria-label={`${node?.display_name ?? nodeId} GID index`}
                                value={binding.gid_index}
                                onChange={(event) => patchBinding(nodeId, { gid_index: event.target.value })}
                                className="h-8 rounded border border-input bg-card px-2 font-mono text-xs"
                              >
                                <option value="">Select GID index</option>
                                {gids.map((gid) => (
                                  <option key={gid.index} value={String(gid.index)}>
                                    {gid.index} · {gid.type || "type unknown"} · {gid.value}
                                  </option>
                                ))}
                              </select>
                            ) : (
                              <Input
                                aria-label={`${node?.display_name ?? nodeId} GID index`}
                                type="number"
                                min={0}
                                max={255}
                                value={binding.gid_index}
                                onChange={(event) => patchBinding(nodeId, { gid_index: event.target.value })}
                                className="h-8 font-mono text-xs"
                              />
                            )}
                          </label>
                        </>
                      ) : null}
                    </div>
                  </article>
                );
              })}
            </section>
          ) : (
            <p className="rounded border border-dashed border-hairline px-3 py-6 text-center text-sm text-muted">
              Select at least two nodes to configure their transport.
            </p>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button onClick={() => void submit()} disabled={!ready || pending}>
            {pending ? "Validating…" : existing ? "Save & revalidate" : "Create & validate"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
