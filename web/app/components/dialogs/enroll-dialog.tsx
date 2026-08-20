import { useState } from "react";
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
  useCreateEnrollmentToken,
  useDeleteEnrollmentToken,
  useEnrollmentTokens,
} from "~/lib/queries";
import { CopyButton } from "~/components/copy-button";
import { shortId } from "~/lib/format";

/**
 * Enroll a node: create a one-use, ten-minute token, show the install
 * command with the plaintext token (shown once), list live tokens.
 */
export function EnrollDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const createToken = useCreateEnrollmentToken();
  const deleteToken = useDeleteEnrollmentToken();
  const { data: tokens } = useEnrollmentTokens(open);
  const [description, setDescription] = useState("");
  const [fresh, setFresh] = useState<{ token: string; expiresAt: string } | null>(null);

  const agentUrl = `https://lmw.tailnet.ts:9443`;

  const onCreate = async () => {
    try {
      const t = await createToken.mutateAsync(description ? { description } : undefined);
      setFresh({ token: t.token ?? "", expiresAt: t.expires_at });
      toast.success("Enrollment token created", {
        description: "Valid for 10 minutes, one use.",
      });
      setDescription("");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "token creation failed");
    }
  };

  const installCommand = (token: string) =>
    `lmw-agent install --server ${agentUrl} --ca-sha256 <fingerprint> --token ${token} --run-as workbench`;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Enroll node
          </DialogTitle>
          <DialogDescription>
            Issue a one-use enrollment token (ten-minute expiry). The agent pins the server CA
            fingerprint and exchanges the token for a node certificate.
          </DialogDescription>
        </DialogHeader>

        {fresh?.token ? (
          <div className="flex flex-col gap-2 rounded border border-primary/40 bg-primary/5 p-3">
            <p className="lmw-label !text-primary">new token — shown once</p>
            <pre className="overflow-auto rounded bg-background/70 p-2 font-mono text-xs leading-relaxed text-ink/90">
              {installCommand(fresh.token)}
            </pre>
            <div className="flex flex-wrap items-center gap-2">
              <CopyButton value={fresh.token} label="token" />
              <CopyButton value={installCommand(fresh.token)} label="install command" className="max-w-full" />
              <span className="ml-auto font-mono text-[11px] text-muted">
                expires {new Date(fresh.expiresAt).toISOString()}
              </span>
            </div>
          </div>
        ) : null}

        <div className="flex flex-col gap-2">
          <Label htmlFor="enroll-desc">Description (optional)</Label>
          <Input
            id="enroll-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="e.g. spark3 replacement"
          />
        </div>

        {tokens && tokens.length > 0 ? (
          <div className="max-h-40 overflow-auto rounded border border-hairline">
            <table className="w-full text-left font-mono text-xs">
              <thead className="sticky top-0 bg-raised text-muted">
                <tr>
                  <th className="px-3 py-1.5 font-medium">id</th>
                  <th className="px-3 py-1.5 font-medium">expires</th>
                  <th className="px-3 py-1.5 font-medium">used</th>
                  <th className="px-3 py-1.5" />
                </tr>
              </thead>
              <tbody>
                {tokens.map((t) => (
                  <tr key={t.id} className="border-t border-hairline">
                    <td className="px-3 py-1.5">{shortId(t.id)}</td>
                    <td className="px-3 py-1.5">{new Date(t.expires_at).toISOString()}</td>
                    <td className="px-3 py-1.5">{t.used_at ? "yes" : "no"}</td>
                    <td className="px-3 py-1.5 text-right">
                      {!t.used_at ? (
                        <Button
                          variant="ghost"
                          size="xs"
                          aria-label={`Revoke token ${shortId(t.id)}`}
                          onClick={() => {
                            void deleteToken.mutateAsync(t.id).then(
                              () => toast.success("Token revoked"),
                              (e) => toast.error(e instanceof Error ? e.message : "revoke failed"),
                            );
                          }}
                        >
                          revoke
                        </Button>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Close
          </Button>
          <Button onClick={() => void onCreate()} disabled={createToken.isPending}>
            {createToken.isPending ? "issuing…" : "Create token"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
