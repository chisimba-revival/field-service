package push

// entityPlannedRoute is the wire name for a planned route.
//
// A planned route records where the guide intends to go, distinct from a trail
// log which records where they actually went. The contract is local-first: the
// device plans the route, and the server may or may not ever receive it.
const entityPlannedRoute = "planned_route"

// entityRouteWaypoint is the wire name for one waypoint along a planned route.
//
// Waypoints are ordered vertices that define the route geometry. POIs are
// optional named waypoints with a kind (gate, waterhole, landmark, custom).
const entityRouteWaypoint = "route_waypoint"
