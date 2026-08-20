import { useState, type ReactNode } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";

/**
 * Generic confirm: returns a trigger button plus the dialog. `onConfirm`
 * is async; pending state disables the buttons.
 */
export function ConfirmDialog({
  title,
  description,
  confirmLabel = "Confirm",
  tone = "default",
  onConfirm,
  children,
}: {
  title: string;
  description?: string;
  confirmLabel?: string;
  tone?: "default" | "destructive";
  onConfirm: () => Promise<void> | void;
  /** Trigger button content. */
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const run = async () => {
    setBusy(true);
    try {
      await onConfirm();
      setOpen(false);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Button
        variant={tone === "destructive" ? "destructive" : "outline"}
        size="sm"
        onClick={() => setOpen(true)}
      >
        {children}
      </Button>
      <Dialog open={open} onOpenChange={(o) => !busy && setOpen(o)}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="font-display text-base font-semibold tracking-wide">
              {title}
            </DialogTitle>
            {description ? <DialogDescription>{description}</DialogDescription> : null}
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)} disabled={busy}>
              Cancel
            </Button>
            <Button
              variant={tone === "destructive" ? "destructive" : "default"}
              onClick={() => void run()}
              disabled={busy}
            >
              {busy ? "working…" : confirmLabel}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
