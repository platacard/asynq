module github.com/platacard/asynq/x

go 1.16

replace github.com/platacard/asynq => ../

require (
	github.com/google/uuid v1.3.0
	github.com/platacard/asynq v0.0.1
	github.com/prometheus/client_golang v1.11.0
	github.com/redis/go-redis/v9 v9.0.3
)
