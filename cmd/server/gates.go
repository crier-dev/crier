package main

import "strings"

// gatedRoute is the single route/gate table used by the runtime fallback and
// the OpenAPI parity test. A gated route may be absent (opt-in surfaces) or
// mounted while refusing requests (operator management surfaces), but its
// public contract always names the configuration value that controls it.
type gatedRoute struct {
	Path string
	Gate string
}

var gatedRoutes = []gatedRoute{
	{Path: "/dagger/runs", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/runs/{id}", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/runs/{id}/cancel", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/runs/{id}/resume", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/runs/{id}/rewind", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/skills/{skill}/run", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/wait", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/waits/{key}/resolve", Gate: "CR_DAGGER_URL"},
	{Path: "/dagger/waits/{key}", Gate: "CR_DAGGER_URL"},
	{Path: "/delivery-log", Gate: "CR_DETECT_ENABLED"},
	{Path: "/delivery-log/verify", Gate: "CR_DETECT_ENABLED"},
	{Path: "/alerts", Gate: "CR_DETECT_ENABLED"},
	{Path: "/canaries", Gate: "CR_DETECT_ENABLED"},
	{Path: "/agents/{id}/kill-switch", Gate: "CR_DETECT_ENABLED"},
	{Path: "/principals", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
	{Path: "/bindings", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
	{Path: "/grants", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
	{Path: "/grants/{grantid}/revoke", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
	{Path: "/trust/anchors", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
	{Path: "/trust/delegations", Gate: "CR_PERMISSIONS_ADMIN_TOKEN"},
}

// gateForRequestPath maps an unrouted concrete request back to the declared
// route shape. Prefix entries cover parameterised route families whose gate is
// independent of the parameter value.
func gateForRequestPath(path string) string {
	for _, route := range gatedRoutes {
		switch route.Path {
		case "/dagger/runs/{id}", "/dagger/runs/{id}/cancel", "/dagger/runs/{id}/resume", "/dagger/runs/{id}/rewind":
			if strings.HasPrefix(path, "/dagger/runs/") {
				return route.Gate
			}
		case "/dagger/skills/{skill}/run":
			if strings.HasPrefix(path, "/dagger/skills/") && strings.HasSuffix(path, "/run") {
				return route.Gate
			}
		case "/dagger/waits/{key}/resolve":
			if strings.HasPrefix(path, "/dagger/waits/") && strings.HasSuffix(path, "/resolve") {
				return route.Gate
			}
		case "/dagger/waits/{key}":
			if strings.HasPrefix(path, "/dagger/waits/") {
				return route.Gate
			}
		case "/agents/{id}/kill-switch":
			if strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/kill-switch") {
				return route.Gate
			}
		case "/grants/{grantid}/revoke":
			if strings.HasPrefix(path, "/grants/") && strings.HasSuffix(path, "/revoke") {
				return route.Gate
			}
		default:
			if path == route.Path {
				return route.Gate
			}
		}
	}
	return ""
}
