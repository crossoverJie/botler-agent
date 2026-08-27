# botler-agent release helper
#
# The deployment model is git-tag based: `npm run restart -- stable` checks out
# the latest GitHub Release tag and runs `npm install`. So a "release" is just a
# semver bump + an annotated tag push; the Release workflow (validate -> draft ->
# publish) takes over from there.
#
# Usage:
#   make release VERSION=0.2.0   bump package.json, commit, tag v0.2.0, push
#   make tag VERSION=0.2.0       alias for release
#   make current                print the current version

VERSION ?=
TAG_PREFIX := v

.PHONY: release tag current check-clean

current:
	@node -p "require('./package.json').version"

# Refuse to release with a dirty working tree so the tag always points at a clean state.
check-clean:
	@git diff --quiet || { echo "error: working tree has uncommitted changes"; exit 1; }
	@git diff --cached --quiet || { echo "error: index has staged changes"; exit 1; }

release: check-clean
	@test -n "$(VERSION)" || { echo "error: pass VERSION=x.y.z (e.g. make release VERSION=0.2.0)"; exit 1; }
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' \
		|| { echo "error: VERSION must be semver (e.g. 0.2.0)"; exit 1; }
	@sed -i.bak -E 's/^([[:space:]]*"version"[[:space:]]*:[[:space:]]*")[^"]*(".*)$$/\1$(VERSION)\2/' package.json \
		&& rm -f package.json.bak
	@git add package.json
	@git commit -m "release: v$(VERSION)"
	@git tag -a "$(TAG_PREFIX)$(VERSION)" -m "v$(VERSION)"
	@git push origin HEAD --tags
	@echo "pushed v$(VERSION); the Release workflow will validate and publish the GitHub Release."

tag: release
