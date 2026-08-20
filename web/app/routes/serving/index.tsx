import { Navigate } from "react-router";

/** Serving section root: land on the deployments table. */
export default function ServingIndex() {
  return <Navigate to="/serving/deployments" replace />;
}
