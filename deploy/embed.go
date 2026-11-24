// Package deploy carries the server deployment files so that
// `rcc setup-server` can install them on a new host.
package deploy

import _ "embed"

//go:embed compose.gateway.yml
var ComposeGateway string

//go:embed compose.user.yml
var ComposeUser string
