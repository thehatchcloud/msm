# Dependency and license review (P17)

`msm` is released under the GPLv3 (`LICENSE.markdown`, shipped in every
archive) and keeps the upstream copyright and attribution (Ahtenus' Minecraft
Init Script, `msmhq/msm`). Every Go module in the build is below; `go test
./compatibility` (`TestDependenciesAreReviewed`) fails when `go.mod` gains or
changes a module that this page does not list, so a new dependency needs a
reviewed edit here.

Reviewed against the module sources at the versions pinned in `go.mod`
(2026-11-05). All licenses are permissive and compatible with distribution
under the GPLv3 (Apache-2.0 is compatible with GPLv3).

| Module | Version | License | Used for |
|---|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 | command tree, help, completion |
| `github.com/spf13/pflag` | v1.0.10 | BSD-3-Clause | flags |
| `github.com/spf13/viper` | v1.21.0 | MIT | native configuration |
| `golang.org/x/sys` | v0.44.0 | BSD-3-Clause | OS calls |
| `github.com/fsnotify/fsnotify` | v1.9.0 | BSD-3-Clause | viper (not used for watching) |
| `github.com/go-viper/mapstructure/v2` | v2.4.0 | MIT | viper |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | Apache-2.0 | cobra (Windows only; not built for our targets) |
| `github.com/pelletier/go-toml/v2` | v2.2.4 | MIT | viper |
| `github.com/sagikazarmark/locafero` | v0.11.0 | MIT | viper |
| `github.com/sourcegraph/conc` | v0.3.1-0.20240121214520-5f936abd7ae8 | MIT | viper |
| `github.com/spf13/afero` | v1.15.0 | Apache-2.0 | viper |
| `github.com/spf13/cast` | v1.10.0 | MIT | viper |
| `github.com/subosito/gotenv` | v1.6.0 | MIT | viper |
| `go.yaml.in/yaml/v3` | v3.0.4 | MIT and Apache-2.0 | viper |
| `golang.org/x/text` | v0.39.0 | BSD-3-Clause | viper |

There is no cgo and no vendored C code in the release builds
(`CGO_ENABLED=0`); only the race tests use cgo. Vulnerabilities are checked
for the application and the standard library with the pinned `govulncheck`
(`make vuln`, the `Go vulnerabilities` CI job) and must be clean for a release
([release.md](release.md)).
