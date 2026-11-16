// Package router is the data-plane reverse proxy. It maps <slug>.apps.<domain>
// to the loopback port of the app's running container (ADR 014) and keeps its
// route table in step with Postgres through NOTIFY route_changes.
package router
