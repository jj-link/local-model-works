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
import { useCreateFabric, useNodes } from "~/lib/queries";
import { stateInfo, TONE_TEXT } from "~/lib/format";
import { cn } from "~/lib/utils";

/**
 * Create a fabric: name, transport, ordered member selection (index is
 * the fabric rank), interface/address, optional RDMA device.
 */
export function CreateFabricDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const { data: nodes } = useNodes();
  const create = useCreateFabric();
  const [name, setName] = useState("");
  const [transport, setTransport] = useState<"roce" | "tcp">("roce");
  const [members, setMembers] = useState<string[]>([]);
  const [interfaceName, setInterfaceName] = useState("");
  const [address, setAddress] = useState("");
  const [rdmaDevice, setRdmaDevice] = useState("");

  useEffect(() => {
    if (open) {
      setName("");
      setTransport("roce");
      setMembers([]);
      setInterfaceName("");
      setAddress("");
      setRdmaDevice("");
      create.reset();
    }
  }, [open, create]);

  const toggleMember = (id: string) => {
    setMembers((prev) => (prev.includes(id) ? prev.filter((m) => m !== id) : [...prev, id]));
  };

  const move = (idx: number, dir: -1 | 1) => {
    setMembers((prev) => {
      const next = [...prev];
      const j = idx + dir;
      if (j < 0 || j >= next.length) return prev;
      [next[idx], next[j]] = [next[j], next[idx]];
      return next;
    });
  };

  const firstMember = nodes?.find((n) => n.id === members[0]);
  const rdmaDevices =
    transport === "roce" ? (firstMember?.inventory?.rdma_devices ?? []) : [];

  const onSubmit = async () => {
    if (!name || members.length < 2) return;
    try {
      const f = await create.mutateAsync({
        name,
        transport,
        members,
        ...(interfaceName ? { interface_name: interfaceName } : {}),
        ...(address ? { address } : {}),
        ...(rdmaDevice ? { rdma_device: rdmaDevice } : {}),
      });
      toast.success("Fabric created", {
        description: `${f.name} · ${f.state}`,
      });
      onOpenChange(false);
      navigate(`/fleet/fabrics/${f.id}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "fabric creation failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Create fabric
          </DialogTitle>
          <DialogDescription>
            Members are ordered: the list index is the fabric rank. The fabric is validated
            against each member's reported interfaces and RDMA devices.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="fabric-name">Name</Label>
              <Input
                id="fabric-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="spark-p2p"
                className="font-mono text-xs"
              />
            </div>
            <div className="grid gap-2">
              <Label>Transport</Label>
              <Select value={transport} onValueChange={(v) => setTransport(v as "roce" | "tcp")}>
                <SelectTrigger className="w-full" aria-label="Transport">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="roce">roce</SelectItem>
                  <SelectItem value="tcp">tcp</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label>Members (rank order)</Label>
            <div className="max-h-52 overflow-auto rounded border border-hairline" role="group" aria-label="Fabric members">
              {(nodes ?? []).map((n) => {
                const idx = members.indexOf(n.id);
                const on = idx !== -1;
                return (
                  <div
                    key={n.id}
                    className={cn(
                      "flex items-center gap-2 border-b border-hairline px-3 py-2 last:border-b-0",
                      on && "bg-raised",
                    )}
                  >
                    <button
                      type="button"
                      onClick={() => toggleMember(n.id)}
                      aria-pressed={on}
                      className={cn(
                        "control flex h-4 w-4 items-center justify-center rounded-sm border",
                        on ? "border-primary bg-primary/20 text-primary" : "border-hairline text-transparent",
                      )}
                      aria-label={`Select ${n.display_name}`}
                    >
                      ✓
                    </button>
                    <span className={cn("font-mono text-xs", on ? "text-foreground" : "text-muted")}>
                      {n.display_name}
                    </span>
                    <span className={cn("text-[10px]", TONE_TEXT[stateInfo(n.status).tone])}>
                      {stateInfo(n.status).label}
                    </span>
                    {on ? (
                      <span className="ml-auto flex items-center gap-1 font-mono text-[11px] text-muted">
                        rank {idx}
                        <button type="button" onClick={() => move(idx, -1)} aria-label={`Move ${n.display_name} up`} className="rounded border border-hairline px-1 hover:text-foreground control">
                          ↑
                        </button>
                        <button type="button" onClick={() => move(idx, 1)} aria-label={`Move ${n.display_name} down`} className="rounded border border-hairline px-1 hover:text-foreground control">
                          ↓
                        </button>
                      </span>
                    ) : null}
                  </div>
                );
              })}
            </div>
            <p className="font-mono text-[11px] text-muted">
              {members.length}/2+ selected
            </p>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="fabric-iface">Interface (optional)</Label>
              <Input
                id="fabric-iface"
                value={interfaceName}
                onChange={(e) => setInterfaceName(e.target.value)}
                placeholder="eth1"
                className="font-mono text-xs"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="fabric-addr">First member address (optional)</Label>
              <Input
                id="fabric-addr"
                value={address}
                onChange={(e) => setAddress(e.target.value)}
                placeholder="10.0.0.2"
                className="font-mono text-xs"
              />
            </div>
          </div>

          {transport === "roce" ? (
            <div className="grid gap-2">
              <Label>RDMA device (optional)</Label>
              <Select
                value={rdmaDevice}
                onValueChange={setRdmaDevice}
                disabled={rdmaDevices.length === 0}
              >
                <SelectTrigger className="w-full" aria-label="RDMA device">
                  <SelectValue placeholder={rdmaDevices.length === 0 ? "auto-detect" : "select device"} />
                </SelectTrigger>
                <SelectContent>
                  {rdmaDevices.map((d) => (
                    <SelectItem key={d.name} value={d.name}>
                      {d.name}
                      {d.vendor ? ` (${d.vendor})` : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : null}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={!name || members.length < 2 || create.isPending}
          >
            {create.isPending ? "creating…" : "Create fabric"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
