// Package httphdr centralizes canonical HTTP header-name constants used across
// edgequota. Defining each name once keeps the same literal from being repeated
// across packages (which the goconst linter reports module-wide) and turns a
// header-name typo into a compile error instead of a silent mismatch.
package httphdr

// Canonical HTTP header names. Values match net/textproto canonicalization, so
// they are safe as both map keys and http.Header.Get/Set arguments.
const (
	Authorization = "Authorization"
	Cookie        = "Cookie"
	XAPIKey       = "X-Api-Key" //nolint:gosec // G101: HTTP header name, not a credential value.
	ContentType   = "Content-Type"
	XForwardedFor = "X-Forwarded-For"
)
