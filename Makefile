COMPOSE := docker compose -f docker/docker-compose.yml

.PHONY: up down migrate-up migrate-down migrate-down-one migrate-create migrate-version psql

## Start Postgres only
up:
	$(COMPOSE) up -d postgres

## Stop everything
down:
	$(COMPOSE) down

## Apply all pending migrations
migrate-up:
	$(COMPOSE) --profile tools run --rm migrate up

## Roll back ALL migrations (careful — asks nothing, just does it)
migrate-down:
	$(COMPOSE) --profile tools run --rm migrate down -all

## Roll back exactly one migration
migrate-down-one:
	$(COMPOSE) --profile tools run --rm migrate down 1

## Show current migration version (and whether it's dirty)
migrate-version:
	$(COMPOSE) --profile tools run --rm migrate version

## Scaffold a new migration pair: make migrate-create name=add_users_table
migrate-create:
	$(COMPOSE) --profile tools run --rm --entrypoint migrate migrate \
		create -ext sql -dir /migrations -seq $(name)

## Quick psql shell into the running container
psql:
	docker exec -it transacta_db psql -U transacta -d transacta