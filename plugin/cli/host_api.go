package plugin

import "github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"

// These types are re-exported from wasmhost for convenience.
// WASM plugins import them via the "host" module; Go host code uses
// plugin.Keyring / plugin.HTTPClient / plugin.Logger directly.

type Keyring = wasmhost.Keyring
type HTTPClient = wasmhost.HTTPClient
type Logger = wasmhost.Logger
