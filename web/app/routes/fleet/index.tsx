import { Navigate } from "react-router";

/** Fleet section root: land on the nodes table. */
export default function FleetIndex() {
  return <Navigate to="/fleet/nodes" replace />;
}
