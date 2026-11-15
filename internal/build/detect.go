package build

import "errors"

// Runtime is a supported build template. Values match apps.runtime in the DB.
type Runtime string

const (
	Python Runtime = "python"
	Node   Runtime = "node"
	Static Runtime = "static"
)

var (
	ErrNoRuntime = errors.New("could not detect the app's runtime: add requirements.txt or pyproject.toml (Python), " +
		"package.json (Node) or index.html (static site) at the top level, or set runtime in shipyard.toml")
	ErrAmbiguousRuntime = errors.New("found both Python (requirements.txt/pyproject.toml) and Node (package.json) files " +
		"at the top level: set runtime in shipyard.toml to choose one")
)

// Detect picks the runtime from the paths in a normalised source archive
// (APP-5). Only top-level files count. Python and Node markers take
// precedence over index.html, which many Python and Node projects also have.
func Detect(paths []string) (Runtime, error) {
	top := make(map[string]bool, len(paths))
	for _, p := range paths {
		top[p] = true
	}
	python := top["requirements.txt"] || top["pyproject.toml"]
	node := top["package.json"]
	switch {
	case python && node:
		return "", ErrAmbiguousRuntime
	case python:
		return Python, nil
	case node:
		return Node, nil
	case top["index.html"]:
		return Static, nil
	}
	return "", ErrNoRuntime
}
