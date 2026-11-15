// Command atlio is an Atlassian Cloud CLI for AI coding agents: it reads Jira
// issues, and reads, creates and updates Confluence pages, through local
// files, without the body passing through the model.
package main

import (
	"os"

	"github.com/178inaba/atlio/v2/internal/cmd"
)

// version is the release version, set at link time by GoReleaser's
// -X main.version={{.Version}} and empty in a binary built any other way. It
// lives in package main so the -X target carries no module path.
var version string

func main() {
	os.Exit(cmd.Execute(version))
}
