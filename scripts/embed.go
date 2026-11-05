// Package scripts carries the scripts a Manager outside the image needs, such as the desktop app's.
package scripts

import _ "embed"

//go:embed install-runner.sh
var InstallRunner []byte
