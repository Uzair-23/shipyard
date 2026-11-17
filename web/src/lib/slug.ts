// Mirrors ValidateSlug in internal/api/apps.go. The server stays authoritative.
const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,28}[a-z0-9]$/
const RESERVED = new Set([
  'www', 'api', 'admin', 'shipyard', 'app', 'apps', 'dashboard', 'mail', 'static', 'assets',
  'registry', 'auth', 'login', 'docs', 'git', 'metrics', 'status', 'console', 'localhost',
  'internal', 'postgres', 'caddy',
])

/** Returns why the slug is not allowed, or '' if it is. */
export function slugProblem(slug: string): string {
  if (!SLUG_RE.test(slug)) {
    return 'Use 3-30 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen.'
  }
  if (slug.length >= 4 && slug.slice(2, 4) === '--') return 'Hyphens are not allowed in the third and fourth position.'
  if (RESERVED.has(slug)) return 'This slug is reserved; choose another.'
  return ''
}
