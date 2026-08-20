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
import { Label } from "~/components/ui/label";
import { useSetRecipeTrust } from "~/lib/queries";
import type { Recipe } from "~/lib/api";

/**
 * Approve trust: mark a recipe `local` (requires accepting the
 * permission diff) or demote to `untrusted`.
 */
export function TrustDialog({
  open,
  onOpenChange,
  recipe,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  recipe: Recipe;
}) {
  const setTrust = useSetRecipeTrust();
  const [target, setTarget] = useState<"local" | "untrusted">("local");
  const [accepted, setAccepted] = useState(false);

  useEffect(() => {
    if (open) {
      setTarget("local");
      setAccepted(false);
    }
  }, [open]);

  const highRisk = recipe.high_risk ?? [];
  const permissions = recipe.permissions ?? [];

  const onSubmit = async () => {
    try {
      await setTrust.mutateAsync({
        digest: recipe.digest,
        trust_state: target,
        permission_diff_accepted: target === "local" ? accepted : true,
      });
      toast.success(`Recipe marked ${target}`);
      onOpenChange(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "trust update failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Change trust — {recipe.name}@{recipe.version}
          </DialogTitle>
          <DialogDescription>
            Trust gates launchability. Marking a recipe local records your approval of its
            permission set.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2" role="radiogroup" aria-label="Target trust state">
            <Label className="lmw-label">target state</Label>
            {(["local", "untrusted"] as const).map((t) => (
              <button
                key={t}
                type="button"
                role="radio"
                aria-checked={target === t}
                onClick={() => setTarget(t)}
                className={
                  target === t
                    ? "control rounded border border-primary/60 bg-primary/10 px-3 py-2 text-left text-sm text-foreground"
                    : "control rounded border border-hairline bg-raised px-3 py-2 text-left text-sm text-muted hover:text-foreground"
                }
              >
                <span className="font-medium">{t}</span>
                <span className="ml-2 font-mono text-[11px] text-muted">
                  {t === "local" ? "operator approved, launchable" : "inspectable only"}
                </span>
              </button>
            ))}
          </div>

          {permissions.length > 0 || highRisk.length > 0 ? (
            <div className="grid gap-2">
              <Label className="lmw-label">permissions</Label>
              <ul className="flex flex-wrap gap-1.5">
                {permissions.map((p) => (
                  <li
                    key={p}
                    className="rounded border border-hairline bg-raised px-2 py-0.5 font-mono text-[11px] text-ink/90"
                  >
                    {p}
                  </li>
                ))}
                {highRisk.map((p) => (
                  <li
                    key={p}
                    className="rounded border border-fault/50 bg-fault/10 px-2 py-0.5 font-mono text-[11px] text-fault"
                  >
                    {p}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          {target === "local" ? (
            <label className="flex cursor-pointer items-center gap-2 text-sm text-foreground">
              <input
                type="checkbox"
                checked={accepted}
                onChange={(e) => setAccepted(e.target.checked)}
                className="h-4 w-4 accent-[#ffb000]"
              />
              I accept the permission diff
            </label>
          ) : null}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={setTrust.isPending || (target === "local" && !accepted)}
          >
            {setTrust.isPending ? "updating…" : `Mark ${target}`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
