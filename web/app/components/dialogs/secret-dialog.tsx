import { useEffect, useState } from "react";
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
import { useDeleteSecret, usePutSecret } from "~/lib/queries";
import type { Secret } from "~/lib/api";

/**
 * Add/replace a secret. Values are write-only: the API returns metadata
 * only, and the value is never shown after the dialog closes.
 */
export function SecretDialog({
  open,
  onOpenChange,
  existing,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  existing?: Secret;
}) {
  const put = usePutSecret();
  const del = useDeleteSecret();
  const [name, setName] = useState("");
  const [purpose, setPurpose] = useState<Secret["purpose"]>("huggingface");
  const [value, setValue] = useState("");

  useEffect(() => {
    if (open) {
      setName(existing?.name ?? "");
      setPurpose(existing?.purpose ?? "huggingface");
      setValue("");
      put.reset();
    }
  }, [open, existing, put]);

  const onSubmit = async () => {
    if (!name || !value) return;
    try {
      await put.mutateAsync({ name, purpose, value });
      toast.success(existing ? "Secret replaced" : "Secret created", { description: name });
      onOpenChange(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "secret write failed");
    }
  };

  const onDelete = async () => {
    if (!existing) return;
    try {
      await del.mutateAsync(existing.id);
      toast.success("Secret deleted", { description: existing.name });
      onOpenChange(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "delete failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            {existing ? `Replace secret — ${existing.name}` : "New secret"}
          </DialogTitle>
          <DialogDescription>
            AES-256-GCM at rest; only metadata is ever returned. {existing ? "Provide a new value to replace; leave blank to keep." : ""}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="secret-name">Name</Label>
            <Input
              id="secret-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={!!existing}
              placeholder="hf-token"
              className="font-mono text-xs"
            />
          </div>
          <div className="grid gap-2">
            <Label>Purpose</Label>
            <Select
              value={purpose}
              onValueChange={(v) => setPurpose(v as Secret["purpose"])}
              disabled={!!existing}
            >
              <SelectTrigger className="w-full" aria-label="Purpose">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="huggingface">huggingface</SelectItem>
                <SelectItem value="github">github</SelectItem>
                <SelectItem value="registry">registry</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="secret-value">Value</Label>
            <Input
              id="secret-value"
              type="password"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder={existing ? "new value" : "token"}
              autoComplete="new-password"
              className="font-mono text-xs"
            />
          </div>
        </div>

        <DialogFooter className="sm:justify-between">
          {existing ? (
            <Button variant="destructive" onClick={() => void onDelete()} disabled={del.isPending}>
              Delete
            </Button>
          ) : (
            <span />
          )}
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button onClick={() => void onSubmit()} disabled={!name || !value || put.isPending}>
              {put.isPending ? "saving…" : existing ? "Replace" : "Create"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
