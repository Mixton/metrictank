.PHONY: test bin docker
default:
	$(MAKE) all
test:
	CGO_ENABLED=1 go test -v -race $(shell go list ./... | grep -v /vendor/ | grep -v stacktest)
check:
	$(MAKE) test
bin:
	./scripts/build.sh
bin-race:
	./scripts/build.sh -race
docker:
	./scripts/build_docker.sh

qa: bin
	# regular qa steps (can run directly on code)
	scripts/qa/gofmt.sh
	scripts/qa/go-generate.sh
	scripts/qa/ineffassign.sh
	scripts/qa/misspell.sh
	scripts/qa/gitignore.sh
	scripts/qa/unused.sh
	scripts/qa/vendor.sh
	# qa-post-build steps minus stack tests
	scripts/qa/docs.sh
all:
	$(MAKE) bin
	$(MAKE) docker
	$(MAKE) qa
