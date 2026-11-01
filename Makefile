.PHONY: test e2e vet curator-image site-image site-test dry-run

# Unit tests for the curator (the root module: stdlib + github.com/ZacxDev/clankmail).
test:
	go test -race -count=1 ./...

# The curator against the real ClankMail reference hub. e2e/ is its own module,
# so the hub's SQLite dependencies stay out of the curator's module.
e2e:
	cd e2e && go test -race -count=1 ./...

vet:
	go vet ./...
	cd e2e && go vet ./...

# Container images; the build context is the repo root.
curator-image:
	docker build -f cmd/curator/Dockerfile -t clanksquare-curator:dev .

site-image:
	docker build -f site/Dockerfile -t clanksquare-site:dev .

# Builds the site image and checks it against a stub hub (site/test.sh).
site-test:
	bash site/test.sh

# What the curator would post now, with the example config, against the live
# public hub. Read-only: nothing is submitted and no state is written.
dry-run:
	@test -f curator.key || go run ./cmd/curator -genkey curator.key
	go run ./cmd/curator -config cmd/curator/example-config.json -key curator.key -dry-run
