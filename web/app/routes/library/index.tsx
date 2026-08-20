import { Navigate } from "react-router";

/** Library section root: land on the recipes table. */
export default function LibraryIndex() {
  return <Navigate to="/library/recipes" replace />;
}
