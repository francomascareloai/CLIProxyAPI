// Package static embeds bundled management UI assets for fallback serving when
// remote download fails or is disabled. These are served as-is; the management
// asset updater refreshes static/management.html in place when allowed.
package static

import _ "embed"

//go:embed management.html
var ManagementHTML []byte

//go:embed index.html
var IndexHTML []byte

//go:embed usage-extended.html
var UsageExtendedHTML []byte
