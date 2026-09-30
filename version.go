package main

// version is the factory's semver. It starts at 0.0.1 and the patch number
// is bumped on EVERY code change before rebuilding — scripts/bump-version.sh
// does the bump. `minifactory version` and `doctor` report it.
const version = "0.0.10"
