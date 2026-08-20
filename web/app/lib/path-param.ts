import { useLocation } from "react-router";

// Module pages are mounted under one wildcard host route. React Router does
// not expose descriptor-level :id captures there, so detail pages read the
// final canonical path segment from the actual location.
export function useTailPathParam(): string {
  const { pathname } = useLocation();
  const segment = pathname.split("/").filter(Boolean).at(-1) ?? "";
  try {
    return decodeURIComponent(segment);
  } catch {
    return "";
  }
}
