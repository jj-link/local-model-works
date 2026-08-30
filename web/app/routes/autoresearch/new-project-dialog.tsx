import { useState } from "react";
import { Beaker } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import type { AutoResearchProject } from "~/lib/api";
import { useCreateAutoResearchProject } from "~/lib/queries";

export function NewAutoResearchProjectDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (project: AutoResearchProject) => void;
}) {
  const createProject = useCreateAutoResearchProject();
  const [name, setName] = useState("");
  const [researchQuestion, setResearchQuestion] = useState("");

  const create = () => {
    const trimmedName = name.trim();
    createProject.mutate(
      {
        idea_prompt: researchQuestion.trim(),
        ...(trimmedName ? { name: trimmedName } : {}),
      },
      {
        onSuccess: (project) => {
          onCreated(project);
          setName("");
          setResearchQuestion("");
          onOpenChange(false);
        },
      },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="arf-dialog sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>New AutoResearch project</DialogTitle>
          <DialogDescription>
            Enter the research question the Factory should investigate. Naming the project is optional.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <label>
            Research question
            <textarea
              aria-label="Research question"
              autoFocus
              value={researchQuestion}
              onChange={(event) => setResearchQuestion(event.target.value)}
              placeholder="Can sparse latent world models improve long-horizon robotic planning?"
            />
          </label>
          <label>
            Project name (optional)
            <input
              aria-label="Project name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="Sparse world models"
            />
          </label>
          {createProject.error ? <p className="arf-inline-error" role="alert">{createProject.error.message}</p> : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button disabled={createProject.isPending || researchQuestion.trim() === ""} onClick={create}>
            <Beaker aria-hidden /> {createProject.isPending ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
