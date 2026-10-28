ROOT_DIR:=$(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

proto: internal/proto/asynq.proto
	protoc -I=$(ROOT_DIR)/internal/proto \
				 --go_out=$(ROOT_DIR)/internal/proto \
				 --go_opt=module=github.com/platacard/asynq/internal/proto \
				 $(ROOT_DIR)/internal/proto/asynq.proto

.PHONY: lint
lint:
	golangci-lint run

# Runs the tests against a throwaway standalone redis, see scripts/test.sh.
# Extra go test flags can be passed with TEST_ARGS, e.g. make test-standalone TEST_ARGS='-run KeyPrefix -v'.
.PHONY: test-standalone
test-standalone:
	$(ROOT_DIR)/scripts/test.sh -- $(TEST_ARGS)

# The same as test-standalone, but against a redis cluster with 3 masters.
.PHONY: test-cluster
test-cluster:
	$(ROOT_DIR)/scripts/test.sh --cluster -- $(TEST_ARGS)
