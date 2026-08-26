import { useEffect, useState } from "react";
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
import type { AutoResearchProject, Node } from "~/lib/api";
import { useCreateAutoResearchProject } from "~/lib/queries";

export function NewAutoResearchProjectDialog({
  open,
  onOpenChange,
  nodes,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  nodes: Node[];
  onCreated: (project: AutoResearchProject) => void;
}) {
  const createProject = useCreateAutoResearchProject();
  const [name, setName] = useState("");
  const [ideaPrompt, setIdeaPrompt] = useState("");
  const [runnerNodeId, setRunnerNodeId] = useState("");

  useEffect(() => {
    if (!runnerNodeId) {
      const online = nodes.find((node) => node.status === "online");
      if (online) setRunnerNodeId(online.id);
    }
  }, [nodes, runnerNodeId]);

  const create = () => {
    createProject.mutate(
      {
        name: name.trim(),
        idea_prompt: ideaPrompt.trim() || undefined,
        runner_node_id: runnerNodeId || undefined,
      },
      {
        onSuccess: (project) => {
          onCreated(project);
          setName("");
          setIdeaPrompt("");
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
            Begin with a direct idea, or leave it blank to generate source-backed candidates.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <label>
            Project name
            <input
              aria-label="Project name"
              autoFocus
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="Evidence-aware decoding"
            />
          </label>
          <label>
            Direct idea (optional)
            <textarea
              aria-label="Direct idea"
              value={ideaPrompt}
              onChange={(event) => setIdeaPrompt(event.target.value)}
              placeholder="Leave blank to generate candidates from a prompt and sources."
            />
          </label>
          <label>
            Runner node
            <select aria-label="Runner node" value={runnerNodeId} onChange={(event) => setRunnerNodeId(event.target.value)}>
              <option value="">not configured</option>
              {nodes.map((node) => <option key={node.id} value={node.id}>{node.display_name} · {node.status}</option>)}
            </select>
          </label>
          {createProject.error ? <p className="arf-inline-error" role="alert">{createProject.error.message}</p> : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button disabled={createProject.isPending || name.trim() === ""} onClick={create}>
            <Beaker aria-hidden /> {createProject.isPending ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
