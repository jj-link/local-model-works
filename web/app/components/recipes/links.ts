import type { RecipeDraft, RecipeRepository } from "~/lib/api";

export function recipePath(digest: string, repositories: RecipeRepository[] = []): string {
  const repository = repositories.find((item) => item.current_recipe?.digest === digest);
  return repository ? `/library/recipes/repositories/${encodeURIComponent(repository.id)}` : `/library/recipes/packages/${encodeURIComponent(digest)}`;
}
export function draftPath(draft: RecipeDraft): string {
  const context = draft.change_context;
  const base = context?.repository_id ? `/library/recipes/repositories/${encodeURIComponent(context.repository_id)}`
    : context?.base_recipe_digest ? `/library/recipes/packages/${encodeURIComponent(context.base_recipe_digest)}`
      : "/library/recipes/new";
  return `${base}?draft=${encodeURIComponent(draft.id)}&section=${context?.kind === "update" ? "updates" : "configuration"}`;
}
