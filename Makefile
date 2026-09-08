.PHONY: run test e2e loadproof fmt check clean

# Boot the app with local Postgres + Redis provisioned by Encore.
run:
	encore run

# Unit and service-level integration tests (Encore provisions isolated test databases).
test:
	encore test ./... -count=1

# End-to-end suite. Boots the app, waits for /health, runs the HTTP tests, tears down.
e2e:
	./scripts/e2e.sh

# The load proof: sell out a 5,000-seat event under concurrency and assert the
# central invariant. Separate from `e2e` because it takes tens of seconds.
loadproof:
	./scripts/loadproof.sh

fmt:
	go fmt ./...

# Everything CI would run.
check: fmt test e2e

clean:
	rm -rf ./tmp
