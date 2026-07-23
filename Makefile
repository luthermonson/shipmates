.PHONY: shipmates-cgroup-launcher shipmates-cgroup-launcher-verify shipmates-m3-provision shipmates-m3-qualifier-run shipmates-installer-payloads shipmates-installer-manifest shipmates-installer-check shipmates-release

SHIPMATES_CGROUP_LAUNCHER_VERSION := shipmates-cgroup-launcher-v1
SHIPMATES_CGROUP_LAUNCHER := dist/shipmates-cgroup-launcher
SHIPMATES_RUNTIME_RELEASE := shipmates-runtime-v1

shipmates-cgroup-launcher:
	@mkdir -p dist
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o $(SHIPMATES_CGROUP_LAUNCHER) ./tools/shipmates-cgroup-launcher
	@chmod 0755 $(SHIPMATES_CGROUP_LAUNCHER)

shipmates-cgroup-launcher-verify: shipmates-cgroup-launcher
	@sha256sum -c tools/shipmates-cgroup-launcher/manifest.sha256

shipmates-m3-provision:
	@mkdir -p dist
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o dist/shipmates-m3-provision ./cmd/shipmates-m3-provision
	@chmod 0755 dist/shipmates-m3-provision

shipmates-m3-qualifier-run:
	@mkdir -p dist
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o dist/shipmates-m3-qualifier-run ./cmd/shipmates-m3-qualifier-run
	@chmod 0755 dist/shipmates-m3-qualifier-run

shipmates-installer-payloads:
	@tools/generate-installer-payloads.sh

# Emit the same manifest that the binary validates at install time. The file
# is an offline audit/release input; it does not install or contact a host.
shipmates-installer-manifest:
	@mkdir -p dist
	@go run ./cmd/shipmates-installer-manifest > dist/shipmates-installer-manifest.json

shipmates-installer-check:
	@bash scripts/check-installer-package.sh

# Offline reproducible runtime archive for the public `sudo shipmates install`
# workflow. This target only writes dist/ and never installs, starts,
# provisions, contacts Fleet, or invokes a service manager. SOURCE_DATE_EPOCH
# is required so archive bytes and checksums are reproducible.
shipmates-release:
	@test -n "$(SOURCE_DATE_EPOCH)" || (echo "SOURCE_DATE_EPOCH is required" >&2; exit 2)
	@rm -rf dist/$(SHIPMATES_RUNTIME_RELEASE)
	@mkdir -p dist/$(SHIPMATES_RUNTIME_RELEASE)
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o dist/$(SHIPMATES_RUNTIME_RELEASE)/shipmates ./
	@go run ./cmd/shipmates-installer-manifest > dist/$(SHIPMATES_RUNTIME_RELEASE)/shipmates-installer-manifest.json
	@cp README.md LICENSE docs/installer-platforms.md dist/$(SHIPMATES_RUNTIME_RELEASE)/
	@chmod 0755 dist/$(SHIPMATES_RUNTIME_RELEASE)/shipmates
	@(cd dist && sha256sum $(SHIPMATES_RUNTIME_RELEASE)/* > $(SHIPMATES_RUNTIME_RELEASE).sha256)
	@tar --sort=name --mtime=@$(SOURCE_DATE_EPOCH) --owner=0 --group=0 --numeric-owner -C dist -czf dist/$(SHIPMATES_RUNTIME_RELEASE).tar.gz $(SHIPMATES_RUNTIME_RELEASE)
