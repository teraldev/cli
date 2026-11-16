// Package schema embeds the JSON Schema of teral.toml, published at
// https://teral.dev/schema/teral.json, so `teral config validate` works
// offline. Refresh it from the platform repo (pkg/teralconfig/schema.json)
// when the format changes.
package schema

import _ "embed"

// URL is where the schema is published.
const URL = "https://teral.dev/schema/teral.json"

//go:embed teral.json
var JSON []byte
